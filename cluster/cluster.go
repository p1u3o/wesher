package cluster

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"time"
	"net"
	"github.com/costela/wesher/common"
	"github.com/hashicorp/memberlist"
	"github.com/mattn/go-isatty"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// KeyLen is the fixed length of cluster keys, must be checked by callers
const KeyLen = 32

// Cluster represents a running cluster configuration
type Cluster struct {
	name      string
	ml        *memberlist.Memberlist
	mlConfig  *memberlist.Config
	localNode *common.Node
	LocalName string
	state     *state
	events    chan memberlist.NodeEvent
}

// New is used to create a new Cluster instance
// The returned instance is ready to be updated with the local node settings then joined
func New(name string, init bool, clusterKey []byte, bindAddr string, bindPort int, advertiseAddr string, advertisePort int, useIPAsName bool) (*Cluster, error) {
	state := &state{}
	if !init {
		loadState(state, name)
	}

	clusterKey, err := computeClusterKey(state, clusterKey)
	if err != nil {
		return nil, fmt.Errorf("computing cluster key: %w", err)
	}

	mlConfig := memberlist.DefaultWANConfig()
	mlConfig.LogOutput = logrus.StandardLogger().WriterLevel(logrus.DebugLevel)
	mlConfig.SecretKey = clusterKey
	mlConfig.BindAddr = bindAddr
	mlConfig.BindPort = bindPort
	mlConfig.AdvertiseAddr = advertiseAddr
	mlConfig.AdvertisePort = advertisePort
	
	if useIPAsName && bindAddr != "0.0.0.0" {
		mlConfig.Name = bindAddr
	}

	ml, err := memberlist.Create(mlConfig)
	if err != nil {
		return nil, fmt.Errorf("creating memberlist: %w", err)
	}

	cluster := Cluster{
		name:      name,
		ml:        ml,
		mlConfig:  mlConfig,
		LocalName: ml.LocalNode().Name,
		// The big channel buffer is a work-around for https://github.com/hashicorp/memberlist/issues/23
		// More than this many simultaneous events will deadlock cluster.members()
		events: make(chan memberlist.NodeEvent, 100),
		state:  state,
	}
	return &cluster, nil
}

// Name provides the current cluster name
func (c *Cluster) Name() string {
	return c.localNode.Name
}

// Join tries to join the cluster by contacting provided ips.
// If no ip is provided, ips of known nodes are used instead.
// Only addresses that are not already members are joined.
func (c *Cluster) Join(hosts []string) error {
	addrs := make([]net.IP, 0, len(hosts))

	// resolve hostnames so we are able to proerly filter out
	// cluster members later
	for _, host := range hosts {
		if addr := net.ParseIP(host); addr != nil {
			addrs = append(addrs, addr)
		} else if ips, err := net.LookupIP(host); err == nil {
			addrs = append(addrs, ips...)
		}
	}

	// add known hosts if necessary
	if len(addrs) == 0 {
		for _, n := range c.state.Nodes {
			addrs = append(addrs, n.Addr)
				}
			}
		
			// filter out addresses that are already members
			targets := make([]string, 0, len(addrs))
			members := c.ml.Members()
		AddrLoop:
			for _, addr := range addrs {
				for _, member := range members {
					if member.Addr.Equal(addr) {
						continue AddrLoop
					}
				}
				targets = append(targets, addr.String())
			}
		
			// finally try and join any remaining address
			if _, err := c.ml.Join(targets); err != nil {
				return fmt.Errorf("joining cluster: %w", err)
			} else if len(targets) > 0 && c.ml.NumMembers() < 2 {
		return errors.New("could not join to any of the provided addresses")
	}
	return nil
}

// Leave saves the current state before leaving, then leaves the cluster
func (c *Cluster) Leave() {
	c.state.save(c.name)
	c.ml.Leave(10 * time.Second)
	c.ml.Shutdown() //nolint: errcheck
}

// Update gossips the local node configuration, propagating any change
func (c *Cluster) Update(localNode *common.Node) {
	c.localNode = localNode
	// wrap in a delegateNode instance for memberlist.Delegate implementation
	delegate := &delegateNode{c.localNode}
	c.mlConfig.Conflict = delegate
	c.mlConfig.Delegate = delegate
	c.mlConfig.Events = &memberlist.ChannelEventDelegate{Ch: c.events}
	c.ml.UpdateNode(1 * time.Second) // we currently do not update after creation
}

// Members provides a channel notifying of cluster changes
// Everytime a change happens inside the cluster (except for local changes),
// the updated list of cluster nodes is pushed to the channel.
func (c *Cluster) Members() <-chan []common.Node {
	changes := make(chan []common.Node)
	go func() {
		for {
			event := <-c.events
			if event.Node.Name == c.LocalName {
				// ignore events about ourselves
				continue
			}
			switch event.Event {
			case memberlist.NodeJoin:
				logrus.Infof("node %s joined", event.Node)
			case memberlist.NodeUpdate:
				logrus.Infof("node %s updated", event.Node)
			case memberlist.NodeLeave:
				logrus.Infof("node %s left", event.Node)
			}

			nodes := make([]common.Node, 0)
			for _, n := range c.ml.Members() {
				if n.Name == c.LocalName {
					continue
				}
				nodes = append(nodes, common.Node{
					Name: n.Name,
					Addr: n.Addr,
					Meta: n.Meta,
				})
			}
			c.state.Nodes = nodes
			changes <- nodes
			c.state.save(c.name) // nolint: errcheck // opportunistic
		}
	}()
	return changes
}

func computeClusterKey(state *state, clusterKey []byte) ([]byte, error) {
	if len(clusterKey) == 0 {
		clusterKey = state.ClusterKey
	}
	if len(clusterKey) == 0 {
		clusterKey = make([]byte, KeyLen)
		_, err := rand.Read(clusterKey)
		if err != nil {
			return nil, fmt.Errorf("reading random source: %w", err)
		}
		// TODO: refactor this into subcommand ("showkey"?)
		if isatty.IsTerminal(os.Stdout.Fd()) {
			fmt.Printf("new cluster key generated: %s\n", base64.StdEncoding.EncodeToString(clusterKey))
		}
	}
	state.ClusterKey = clusterKey
	return clusterKey, nil
}
