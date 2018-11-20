// Copyright © 2017 The virtual-kubelet authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Modification copyright @ 2018 <Name> <E-mail>

package cmd

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/cpuguy83/strongerrors"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/ordovicia/kubernetes-scheduler-simulator/log"
	"github.com/ordovicia/kubernetes-scheduler-simulator/sim"
)

var configFile string
var config = Config{
	Cluster: ClusterConfig{Nodes: []NodeConfig{}},
	Taint: TaintConfig{
		Key:    "kubernetes-scheduler-simulator.io/kubelet",
		Value:  "simulator",
		Effect: "NoSchedule",
	},
	APIPort:     10250,
	MetricsPort: 10255,
	LogLevel:    "info",
}
var nodeConfigs []sim.NodeConfig

// RootCmd represents the base command when called without any subcommands
var RootCmd = &cobra.Command{
	Use:   "kubernetes-scheduler-simulator",
	Short: "kubernetes-scheduler-simulator provides a virtual kubernetes cluster interface for your kubernetes scheduler.",
	Long: `FIXME: virtual-kubelet implements the Kubelet interface with a pluggable
backend implementation allowing users to create kubernetes nodes without running the kubelet.
This allows users to schedule kubernetes workloads on nodes that aren't running Kubernetes.`,
	Run: func(cmd *cobra.Command, args []string) {
		_, cancel := context.WithCancel(context.Background())

		for _, nodeConfig := range nodeConfigs {
			log.L.Infof("node %q started", nodeConfig.Name)
			node := sim.NewNode(nodeConfig)
			_ = node
			time.Sleep(1 * time.Second)
		}

		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

		go func() {
			<-sig
			cancel()
		}()
	},
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	if err := RootCmd.Execute(); err != nil {
		log.G(context.TODO()).WithError(err).Fatal("Error executing root command")
	}
}

func init() {
	cobra.OnInitialize(readConfig)
	RootCmd.PersistentFlags().StringVar(&configFile, "config", "", "config file (excluding file extension)")
}

func readConfig() {
	// TODO: Do not try to read config when 'help' or 'version' subcommand is provided

	viper.SetConfigName(configFile)
	viper.AddConfigPath(".")

	if err := viper.ReadInConfig(); err != nil {
		log.G(context.TODO()).WithError(err).Fatal("Error reading config file")
	} else {
		log.G(context.TODO()).Debugf("Using config file %s", viper.ConfigFileUsed())
	}

	if err := viper.Unmarshal(&config); err != nil {
		log.G(context.TODO()).WithError(err).Fatal("Error decoding config")
	}

	level, err := log.ParseLevel(config.LogLevel)
	if err != nil {
		log.G(context.TODO()).WithField("logLevel", config.LogLevel).Fatal("log level is not supported")
	}
	logrus.SetLevel(level)

	logger := log.L
	log.L = logger

	taint, err := buildTaint(config.Taint)
	if err != nil {
		logger.WithError(err).Fatal("Error building taint")
	}

	logger.Debugf("Config %+v", config)

	for _, node := range config.Cluster.Nodes {
		capacity, err := buildCapacity(node.Capacity)
		if err != nil {
			logger.WithError(err).Fatal("Error building capacity")
		}

		nodeConfig := sim.NodeConfig{
			Name:            node.Name,
			Capacity:        capacity,
			OperatingSystem: node.OperatingSystem,
			Taint:           *taint,
		}

		nodeConfigs = append(nodeConfigs, nodeConfig)
	}

	logger.Debugf("nodeConfigs: %+v", nodeConfigs)
}

func buildTaint(config TaintConfig) (*v1.Taint, error) {
	var effect v1.TaintEffect
	switch config.Effect {
	case "NoSchedule":
		effect = v1.TaintEffectNoSchedule
	case "NoExecute":
		effect = v1.TaintEffectNoExecute
	case "PreferNoSchedule":
		effect = v1.TaintEffectPreferNoSchedule
	default:
		return nil, strongerrors.InvalidArgument(errors.Errorf("taint effect %q is not supported", config.Effect))
	}

	return &v1.Taint{
		Key:    config.Key,
		Value:  config.Value,
		Effect: effect,
	}, nil
}

func buildCapacity(config map[v1.ResourceName]string) (v1.ResourceList, error) {
	resourceList := v1.ResourceList{}

	for key, value := range config {
		quantity, err := resource.ParseQuantity(value)
		if err != nil {
			return nil, strongerrors.InvalidArgument(errors.Errorf("invalid %s value %q", key, value))
		}
		resourceList[key] = quantity
	}

	return resourceList, nil
}