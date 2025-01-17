package main // import "github.com/costela/wesher"

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/cenkalti/backoff"
	"github.com/costela/wesher/cluster"
	"github.com/costela/wesher/common"
	"github.com/costela/wesher/etchosts"
	"github.com/costela/wesher/wg"
	"github.com/sirupsen/logrus"
)

var version = "dev"

func main() {
	// General initialization
	config, err := loadConfig()
	if err != nil {
		logrus.Fatal(err)
	}
	if config.Version {
		fmt.Println(version)
		os.Exit(0)
	}
	logLevel, err := logrus.ParseLevel(config.LogLevel)
	if err != nil {
		logrus.WithError(err).Fatal("could not parse loglevel")
	}
	logrus.SetLevel(logLevel)

	logrus.Infof("\tAdvertiseAddr: %s", config.AdvertiseAddr)

	// Create the wireguard and cluster configuration
	cluster, err := cluster.New(config.Interface, config.Init, config.ClusterKey, config.BindAddr, config.ClusterPort, config.AdvertiseAddr, config.ClusterPort, config.UseIPAsName)
	if err != nil {
		logrus.WithError(err).Fatal("could not create cluster")
	}

	keepaliveDuration, err := time.ParseDuration(config.KeepaliveInterval)
	if err != nil {
		logrus.WithError(err).Fatal("could not parse time duration for keepalive")
	}

	wgstate, localNode, err := wg.New(config.Interface, config.WireguardPort, config.MTU, (*net.IPNet)(config.OverlayNet), cluster.LocalName, &keepaliveDuration)
	if err != nil {
		logrus.WithError(err).Fatal("could not instantiate wireguard controller")
	}

	// Prepare the rejoin timer
	rejoin := make(<-chan time.Time)
	if config.Rejoin > 0 {
		rejoin = time.Tick(time.Duration(1000000000 * config.Rejoin))
	}

	// Prepare the /etc/hosts writer
	hostsFile := &etchosts.EtcHosts{
		Banner: "# ! managed automatically by wesher interface " + config.Interface,
		Logger: logrus.StandardLogger(),
	}

	// Join the cluster
	cluster.Update(localNode)
	nodec := cluster.Members() // avoid deadlocks by starting before join
	if err := backoff.RetryNotify(
		func() error { return cluster.Join(config.Join) },
		backoff.NewExponentialBackOff(),
		func(err error, dur time.Duration) {
			logrus.WithError(err).Errorf("could not join cluster, retrying in %s", dur)
		},
	); err != nil {
		logrus.WithError(err).Fatal("could not join cluster")
	}

	routedNets := make([]*net.IPNet, len(config.RoutedNet))
	for index, routedNetItem := range config.RoutedNet {
		logrus.Debugf("adding network %s", routedNetItem)
		routedNets[index] = (*net.IPNet)(routedNetItem)
	}

	// Main loop
	routesc := common.Routes(routedNets)
	incomingSigs := make(chan os.Signal, 1)
	signal.Notify(incomingSigs, syscall.SIGTERM, os.Interrupt)
	logrus.Debug("waiting for cluster events")
	for {
		select {
		case rawNodes := <-nodec:
			nodes := make([]common.Node, 0, len(rawNodes))
			hosts := make(map[string][]string, len(rawNodes))
			logrus.Info("cluster members:\n")
			for _, node := range rawNodes {

				if err := node.DecodeMeta(); err != nil {
					logrus.Warnf("\t addr: %s, could not decode metadata", node.Addr)
					continue
				}
				logrus.Infof("\taddr: %s, overlay: %s, pubkey: %s, net: %s, routes: %s", node.Addr, node.OverlayAddr, node.PubKey, node.Routes)
				nodes = append(nodes, node)
				hosts[node.OverlayAddr.IP.String()] = []string{node.Name}
			}
			if err := wgstate.SetUpInterface(nodes, routedNets); err != nil {
				logrus.WithError(err).Error("could not up interface")
				wgstate.DownInterface()
			}
			if !config.NoEtcHosts {
				if err := hostsFile.WriteEntries(hosts); err != nil {
					logrus.WithError(err).Error("could not write hosts entries")
				}
			}
			if len(config.NodeUpdateScript) > 0 {
				updateScript, _ := exec.LookPath(config.NodeUpdateScript)
				cmd := &exec.Cmd{
					Path:   updateScript,
					Args:   []string{updateScript, config.Interface},
					Stdout: os.Stdout,
					Stderr: os.Stderr,
				}
				if err := cmd.Run(); err != nil {
					logrus.Errorf("error while executing node-update-script %s: %s", config.NodeUpdateScript, err)
				}
			}
		case routes := <-routesc:
			logrus.Info("announcing new routes...")
			localNode.Routes = routes
			cluster.Update(localNode)
		case <-rejoin:
			logrus.Debug("rejoining missing join nodes...")
			cluster.Join(config.Join)
		case <-incomingSigs:
			logrus.Info("terminating...")
			cluster.Leave()
			if !config.NoEtcHosts {
				if err := hostsFile.WriteEntries(map[string][]string{}); err != nil {
					logrus.WithError(err).Error("could not remove stale hosts entries")
				}
			}
			if err := wgstate.DownInterface(); err != nil {
				logrus.WithError(err).Error("could not down interface")
			}
			os.Exit(0)
		}
	}
}
