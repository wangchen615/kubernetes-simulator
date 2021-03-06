package kubesim

import (
	"context"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/cpuguy83/strongerrors"
	"github.com/pkg/errors"
	"github.com/spf13/viper"
	"k8s.io/api/core/v1"

	"github.com/ordovicia/kubernetes-simulator/api"
	"github.com/ordovicia/kubernetes-simulator/kubesim/clock"
	"github.com/ordovicia/kubernetes-simulator/kubesim/config"
	"github.com/ordovicia/kubernetes-simulator/kubesim/node"
	"github.com/ordovicia/kubernetes-simulator/log"
)

// KubeSim represents a kubernetes cluster simulator.
type KubeSim struct {
	nodes map[string]*node.Node
	pods  podQueue
	tick  int

	submitters []api.Submitter
	filters    []api.Filter
	scorers    []api.Scorer
}

// NewKubeSim creates a new KubeSim with the config.
func NewKubeSim(conf *config.Config) (*KubeSim, error) {
	log.G(context.TODO()).Debugf("Config: %+v", *conf)
	if err := configure(conf); err != nil {
		return nil, errors.Errorf("error configuring: %s", err.Error())
	}

	nodes := map[string]*node.Node{}
	for _, nodeConf := range conf.Cluster.Nodes {
		log.L.Debugf("NodeConfig: %+v", nodeConf)

		nodeV1, err := config.BuildNode(nodeConf, conf.StartClock)
		if err != nil {
			return nil, errors.Errorf("error building node config: %s", err.Error())
		}

		n := node.NewNode(nodeV1)
		nodes[nodeV1.Name] = &n
		log.L.Debugf("Node %q created", nodeV1.Name)
	}

	kubesim := KubeSim{
		nodes:   nodes,
		pods:    podQueue{},
		tick:    conf.Tick,
		filters: []api.Filter{},
		scorers: []api.Scorer{},
	}

	return &kubesim, nil
}

// NewKubeSimFromConfigPath creates a new KubeSim with config from confPath (excluding file path).
func NewKubeSimFromConfigPath(confPath string) (*KubeSim, error) {
	conf, err := readConfig(confPath)
	if err != nil {
		return nil, errors.Errorf("error reading config: %s", err.Error())
	}

	return NewKubeSim(conf)
}

// RegisterSubmitter registers a new submitter plugin to this KubeSim.
func (k *KubeSim) RegisterSubmitter(submitter api.Submitter) {
	k.submitters = append(k.submitters, submitter)
}

// RegisterFilter registers a new filter plugin to this KubeSim.
func (k *KubeSim) RegisterFilter(filter api.Filter) {
	k.filters = append(k.filters, filter)
}

// RegisterScorer registers a new scorer plugin to this KubeSim.
func (k *KubeSim) RegisterScorer(scorer api.Scorer) {
	k.scorers = append(k.scorers, scorer)
}

// Run executes the main loop, which invokes scheduler plugins and schedules queued pods to a
// selected node.
func (k *KubeSim) Run(ctx context.Context) error {
	tick := make(chan clock.Clock)

	go func() {
		clock := clock.NewClock(time.Now())
		for {
			clock = clock.Add(time.Duration(k.tick) * time.Second)
			tick <- clock
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case clock := <-tick:
			log.L.Debugf("Clock %s", clock.String())

			// convert []*node.Node to []*v1.Node
			nodes := []*v1.Node{}
			for _, node := range k.nodes {
				nodes = append(nodes, node.ToV1())
			}

			if err := k.submit(clock, nodes); err != nil {
				return err
			}

			if err := k.scheduleOne(clock, nodes); err != nil {
				return err
			}
		}
	}
}

// submit appends all pods submitted from submitters.
func (k *KubeSim) submit(clock clock.Clock, nodes []*v1.Node) error {
	for _, submitter := range k.submitters {
		pods, err := submitter.Submit(clock, nodes)
		if err != nil {
			return err
		}

		for _, pod := range pods {
			k.pods.append(pod)
		}
	}

	return nil
}

// scheduleOne try to schedule one pod at the front of queue, or return immediately if no pod is in
// the queue.
func (k *KubeSim) scheduleOne(clock clock.Clock, nodes []*v1.Node) error {
	pod, err := k.pods.pop()
	if err == errEmptyPodQueue {
		return nil
	}

	log.L.Tracef("Trying to schedule pod %v", pod)

	if err := k.scheduleOneFilter(pod, nodes); err != nil {
		return err
	}

	nodeSelected, err := k.scheduleOneScore(pod, nodes)
	if err != nil {
		return err
	}
	log.L.Tracef("Selected node %v", nodeSelected)

	if err := nodeSelected.CreatePod(clock, pod); err != nil {
		return err
	}

	return nil
}

func (k *KubeSim) scheduleOneFilter(pod *v1.Pod, nodes []*v1.Node) error {
	for _, filter := range k.filters {
		log.L.Tracef("Filtering nodes %v", nodes)

		nodesOk := []*v1.Node{}
		for _, node := range nodes {
			ok, err := filter.Filter(pod, node)
			if err != nil {
				return err
			}
			if ok {
				nodesOk = append(nodesOk, node)
			}
		}
		nodes = nodesOk

		log.L.Tracef("Filtered nodes %v", nodes)
	}

	return nil
}

func (k *KubeSim) scheduleOneScore(pod *v1.Pod, nodes []*v1.Node) (nodeSelected *node.Node, err error) {
	nodeScore := make(map[string]int)

	for _, scorer := range k.scorers {
		log.L.Tracef("Scoring nodes %v", nodes)

		scores, weight, err := scorer.Score(pod, nodes)
		if err != nil {
			return nil, err
		}

		for _, score := range scores {
			nodeScore[score.Host] += score.Score * weight
		}

		log.L.Tracef("Scored nodes %v", nodeScore)
	}

	scoreMax := -1
	scoreMaxNode := ""
	for node, score := range nodeScore {
		if score > scoreMax {
			scoreMaxNode = node
			scoreMax = score
		}
	}

	nodeSelected, ok := k.nodes[scoreMaxNode]
	if !ok {
		return nil, strongerrors.NotFound(errors.Errorf("node %q not found", scoreMaxNode))
	}

	pod.Spec.NodeName = scoreMaxNode

	return nodeSelected, nil
}

// readConfig reads and parses a config from the path (excluding file extension).
func readConfig(path string) (*config.Config, error) {
	viper.SetConfigName(path)
	viper.AddConfigPath(".")

	if err := viper.ReadInConfig(); err != nil {
		return nil, err
	}
	log.G(context.TODO()).Debugf("Using config file %s", viper.ConfigFileUsed())

	var conf = config.Config{
		LogLevel:   "info",
		Tick:       10,
		StartClock: "",
		// APIPort:     10250,
		// MetricsPort: 10255,
		Cluster: config.ClusterConfig{Nodes: []config.NodeConfig{}},
	}

	if err := viper.Unmarshal(&conf); err != nil {
		return nil, err
	}

	return &conf, nil
}

func configure(conf *config.Config) error {
	level, err := log.ParseLevel(conf.LogLevel)
	if err != nil {
		return strongerrors.InvalidArgument(errors.Errorf("%s: log level %q not supported", err.Error(), level))
	}
	logrus.SetLevel(level)

	logger := log.L
	log.L = logger

	return nil
}
