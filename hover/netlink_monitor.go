// Copyright 2016 PLUMgrid
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hover

import (
	"fmt"
	"strconv"
	"sync"
	"syscall"

	"github.com/vishvananda/netlink"
)

// NetlinkMonitor keeps track of the interfaces on this host. It can invoke a
// callback when an interface is added/deleted.
type NetlinkMonitor struct {
	// receive LinkUpdates from nl.Subscribe
	updates chan netlink.LinkUpdate

	// close(nlDone) to terminate Subscribe loop
	done  chan struct{}
	flush chan struct{}

	// nodes tracks netlink ifindex to graph Node mapping
	nodes map[int]*ExtInterface

	g   Graph
	mtx sync.RWMutex
}

func NewNetlinkMonitor(g Graph) (res *NetlinkMonitor, err error) {
	nlmon := &NetlinkMonitor{
		updates: make(chan netlink.LinkUpdate),
		done:    make(chan struct{}),
		flush:   make(chan struct{}),
		nodes:   make(map[int]*ExtInterface),
		g:       g,
	}
	err = netlink.LinkSubscribe(nlmon.updates, nlmon.done)
	defer func() {
		if err != nil {
			nlmon.Close()
		}
	}()
	if err != nil {
		return
	}
	links, err := netlink.LinkList()
	if err != nil {
		return
	}
	for _, link := range links {
		nlmon.handleNewlink(link)
	}
	Debug.Println("NewNetlinkMonitor DONE")
	go nlmon.ParseLinkUpdates()
	res = nlmon
	return
}

func (nm *NetlinkMonitor) Close() {
	close(nm.done)
}

func (nm *NetlinkMonitor) handleNewlink(link netlink.Link) {
	nm.mtx.Lock()
	defer nm.mtx.Unlock()
	if _, ok := nm.nodes[link.Attrs().Index]; !ok {
		nm.nodes[link.Attrs().Index] = NewExtInterface(link)
	}
}

func (nm *NetlinkMonitor) handleDellink(link netlink.Link) {
	nm.mtx.Lock()
	defer nm.mtx.Unlock()
	if _, ok := nm.nodes[link.Attrs().Index]; ok {
		delete(nm.nodes, link.Attrs().Index)
	}
}

func (nm *NetlinkMonitor) ParseLinkUpdates() {
	for {
		select {
		case update, ok := <-nm.updates:
			if !ok {
				// channel closed
				return
			}
			switch update.Header.Type {
			case syscall.RTM_NEWLINK:
				nm.handleNewlink(update.Link)
			case syscall.RTM_DELLINK:
				nm.handleDellink(update.Link)
			}
		case _ = <-nm.flush:
			// when nm.nodes is queried, ensures that all pending updates have been processed
		}
	}
}

func (nm *NetlinkMonitor) Interfaces() (nodes []InterfaceNode) {
	nm.flush <- struct{}{}
	nm.mtx.RLock()
	defer nm.mtx.RUnlock()
	for _, node := range nm.nodes {
		nodes = append(nodes, node)
	}
	return
}
func (nm *NetlinkMonitor) InterfaceByName(name string) (node InterfaceNode, err error) {
	nm.flush <- struct{}{}
	nm.mtx.RLock()
	defer nm.mtx.RUnlock()
	link, err := netlink.LinkByName(name)
	if err != nil {
		return
	}
	var ok bool
	if node, ok = nm.nodes[link.Attrs().Index]; !ok {
		err = fmt.Errorf("No interface %s found", name)
		return
	}
	return
}

func (nm *NetlinkMonitor) EnsureInterfaces(g Graph, pp *PatchPanel) {
	nm.flush <- struct{}{}
	nm.mtx.Lock()
	defer nm.mtx.Unlock()
	for ifindex, node := range nm.nodes {
		if node.ID() < 0 {
			continue
		}
		Info.Printf("visit: %d :: %s :: %d\n", node.ID(), node.ShortPath(), ifindex)
		pp.modules.Set(strconv.Itoa(node.ID()), strconv.Itoa(node.FD()))
		switch deg := g.Degree(node); deg {
		case 2:
			//Debug.Printf("Adding ingress for %s\n", node.Link().Attrs().Name)
			next := g.From(node)[0].(Node)
			e := g.E(node, next)
			chain, err := NewIngressChain(e.Chain())
			if err != nil {
				panic(err)
			}
			defer chain.Close()
			Info.Printf(" %4d: %-11s{%#x}\n", e.FID(), next.ShortPath(), e.Chain())
			if err := ensureIngressFd(node.Link(), chain.FD()); err != nil {
				panic(err)
			}
		default:
			panic(fmt.Errorf("Invalid # edges for node %s, must be 2, got %d", node.Path(), deg))
		}
	}
}
