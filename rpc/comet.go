// Copyright © 2014 Terry Mao, LiuDing All rights reserved.
// This file is part of gopush-cluster.

// gopush-cluster is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

// gopush-cluster is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.

// You should have received a copy of the GNU General Public License
// along with gopush-cluster.  If not, see <http://www.gnu.org/licenses/>.

package rpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Terry-Mao/gopush-cluster/ketama"
	myzk "github.com/Terry-Mao/gopush-cluster/zk"
	"github.com/golang/glog"
	"github.com/samuel/go-zookeeper/zk"
	"net/rpc"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// protocol of Comet subcription
	cometProtocolUnknown    = 0
	cometProtocolWS         = 1
	cometProtocolWSStr      = "ws"
	cometProtocolTCP        = 2
	cometProtocolTCPStr     = "tcp"
	cometProtocolRPC        = 3
	cometProtocolRPCStr     = "rpc"
	cometService            = "CometRPC"
	CometServicePushPrivate = "CometRPC.PushPrivate"
)

var (
	// Store the first alive Comet service of every node
	// If there is no alive service under the node, the map`s value will be nil, but key is exist in map
	cometNodeInfoMap = make(map[string]*CometNodeInfo)
	// Ketama algorithm for check Comet node
	cometRing   *ketama.HashRing
	ErrCometRPC = errors.New("comet rpc call failed")
)

type CometNodeInfo struct {
	// The addr for subscribe, format like:map[Protocol]Addr
	Addr map[int][]*RPCClient
	// The connection for Comet RPC
	CometRPC *RandLB
	// The comet wieght
	weight int
}

type CometNodeEvent struct {
	// node name(node1, node2...)
	Key string
	// node info
	Value *CometNodeInfo
	// event type
	Event int
}

// Channel Push Private Message Args
type CometPushPrivateArgs struct {
	Key    string          // subscriber key
	Msg    json.RawMessage // message content
	Expire uint            // message expire second
}

// Channel Push Public Message Args
type CometPushPublicArgs struct {
	MsgID int64  // message id
	Msg   string // message content
}

// Channel Migrate Args
type CometMigrateArgs struct {
	Nodes map[string]int // current comet nodes
	Vnode int            // ketama virtual node number
}

// Channel New Args
type CometNewArgs struct {
	Expire int64  // message expire second
	Token  string // auth token
	Key    string // subscriber key
}

func watchCometRoot(conn *zk.Conn, fpath string, ch chan *CometNodeEvent) error {
	for {
		nodes, watch, err := myzk.GetNodesW(conn, fpath)
		if err == myzk.ErrNodeNotExist {
			glog.Warningf("zk don't have node \"%s\", retry in %d second", fpath, waitNodeDelay)
			time.Sleep(waitNodeDelaySecond)
			continue
		} else if err == myzk.ErrNoChild {
			glog.Warningf("zk don't have any children in \"%s\", retry in %d second", fpath, waitNodeDelay)
			for node, _ := range cometNodeInfoMap {
				ch <- &CometNodeEvent{Event: eventNodeDel, Key: node}
			}
			time.Sleep(waitNodeDelaySecond)
			continue
		} else if err != nil {
			glog.Errorf("getNodes error(%v), retry in %d second", err, waitNodeDelay)
			time.Sleep(waitNodeDelaySecond)
			continue
		}
		nodesMap := map[string]bool{}
		// handle new add nodes
		for _, node := range nodes {
			if _, ok := cometNodeInfoMap[node]; !ok {
				ch <- &CometNodeEvent{Event: eventNodeAdd, Key: node}
			}
			nodesMap[node] = true
		}
		// handle delete nodes
		for node, _ := range cometNodeInfoMap {
			if _, ok := nodesMap[node]; !ok {
				ch <- &CometNodeEvent{Event: eventNodeDel, Key: node}
			}
		}
		// blocking wait node changed
		event := <-watch
		glog.Infof("zk path: \"%s\" receive a event %v", fpath, event)
	}
}

// handleCometNodeEvent add and remove CometNodeInfo, copy the src map to a new map then replace the variable.
func handleCometNodeEvent(conn *zk.Conn, fpath string, retry, ping time.Duration, vnode int, ch chan *CometNodeEvent) {
	for {
		ev := <-ch
		// copy map from src
		tmpMap := make(map[string]*CometNodeInfo, len(cometNodeInfoMap))
		for k, v := range cometNodeInfoMap {
			tmpMap[k] = v
		}
		// handle event
		if ev.Event == eventNodeAdd {
			glog.Infof("add node: \"%s\"", ev.Key)
			tmpMap[ev.Key] = nil
			go watchCometNode(conn, ev.Key, fpath, retry, ping, vnode, ch)
		} else if ev.Event == eventNodeDel {
			glog.Infof("del node: \"%s\"", ev.Key)
			delete(tmpMap, ev.Key)
		} else if ev.Event == eventNodeUpdate {
			glog.Infof("update node: \"%s\"", ev.Key)
			tmpMap[ev.Key] = ev.Value
		} else {
			glog.Errorf("unknown node event: %d", ev.Event)
			panic("unknown node event")
		}
		// if exist old node info, destroy
		if info, ok := cometNodeInfoMap[ev.Key]; ok {
			if info != nil {
				info.CometRPC.Destroy()
			}
		}
		// update comet hash, cause node has changed
		tempRing := ketama.NewRing(vnode)
		for k, v := range tmpMap {
			if v != nil {
				tempRing.AddNode(k, v.weight)
			}
		}
		tempRing.Bake()
		// use the tmpMap atomic replace the global cometNodeInfoMap
		cometNodeInfoMap = tmpMap
		cometRing = tempRing
		glog.V(1).Infof("cometNodeInfoMap len: %d", len(cometNodeInfoMap))
	}
}

// watchNode watch a named node for leader selection when failover
func watchCometNode(conn *zk.Conn, node, fpath string, retry, ping time.Duration, vnode int, ch chan *CometNodeEvent) {
	fpath = path.Join(fpath, node)
	for {
		nodes, watch, err := myzk.GetNodesW(conn, fpath)
		if err == myzk.ErrNodeNotExist {
			glog.Warningf("zk don't have node \"%s\"", fpath)
			break
		} else if err == myzk.ErrNoChild {
			glog.Warningf("zk don't have any children in \"%s\", retry in %d second", fpath, waitNodeDelay)
			time.Sleep(waitNodeDelaySecond)
			continue
		} else if err != nil {
			glog.Errorf("zk path: \"%s\" getNodes error(%v), retry in %d second", fpath, err, waitNodeDelay)
			time.Sleep(waitNodeDelaySecond)
			continue
		}
		// leader selection
		// register node
		sort.Strings(nodes)
		if info, err := registerCometNode(conn, nodes[0], fpath, retry, ping, vnode); err != nil {
			glog.Errorf("zk path: \"%s\" registerNode error(%v)", fpath, err)
			time.Sleep(waitNodeDelaySecond)
			continue
		} else {
			// update node info
			ch <- &CometNodeEvent{Event: eventNodeUpdate, Key: node, Value: info}
		}
		// blocking receive event
		event := <-watch
		glog.Infof("zk path: \"%s\" receive a event: (%v)", fpath, event)
	}
	// WARN, if no persistence node and comet rpc not config
	glog.Warningf("zk path: \"%s\" never watch again till recreate", fpath)
}

func registerCometNode(conn *zk.Conn, node, fpath string, retry, ping time.Duration, vnode int) (*CometNodeInfo, error) {
	fpath = path.Join(fpath, node)
	data, _, err := conn.Get(fpath)
	if err != nil {
		glog.Errorf("zk.Get(\"%s\") error(%v)", fpath, err)
		return nil, err
	}
	// fetch and parse comet info
	w, addrs, err := parseCometNode(string(data))
	if err != nil {
		glog.Errorf("parseCometNode(\"%s\") error(%v)", string(data), err)
		return nil, err
	}
	info := &CometNodeInfo{Addr: addrs, weight: w}
	rpcAddr, ok := addrs[cometProtocolRPC]
	if !ok || len(rpcAddr) == 0 {
		glog.Errorf("zk nodes: \"%s\" don't have rpc addr", fpath)
		return nil, ErrCometRPC
	}
	// init comet rpc
	clients := make(map[string]*RPCClient, len(rpcAddr))
	for _, addr := range rpcAddr {
		r, err := rpc.Dial("tcp", addr.Addr)
		if err != nil {
			glog.Errorf("rpc.Dial(\"%s\") error(%v)", addr.Addr, err)
			return nil, err
		}
		addr.Client = r
		clients[addr.Addr] = addr
	}
	lb, err := NewRandLB(clients, cometService, retry, ping, vnode, true)
	if err != nil {
		glog.Errorf("NewRandLR() error(%v)", err)
		panic(err)
	}
	info.CometRPC = lb
	glog.Infof("zk path: \"%s\" register nodes: \"%s\"", fpath, node)
	return info, nil
}

// parseCometNode parse the protocol data, the data format like: 1;ws://ip:port1,tcp://ip:port2,rpc://ip:port3
func parseCometNode(data string) (w int, res map[int][]*RPCClient, err error) {
	dataArr := strings.Split(data, ";") // eg: 1;tcp://1-ip:port,ws://1-ip:port
	if len(dataArr) != 2 {
		err = fmt.Errorf("data:\"%s\" format error", data)
		return
	}
	w, err = strconv.Atoi(dataArr[0])
	if err != nil {
		err = fmt.Errorf("data:\"%s\" format error(%v)", data, err)
		return
	}
	protoArr := strings.Split(dataArr[1], ",") // eg tcp://1-ip:port,ws://1-ip:port
	res = make(map[int][]*RPCClient, len(protoArr))
	for i := 0; i < len(protoArr); i++ {
		addrArr := strings.Split(protoArr[i], "://") // eg: tcp://1-ip:port
		if len(addrArr) != 2 {
			err = fmt.Errorf("data:\"%s\" format error", data)
			res = nil
			return
		}
		proto := cometProtoInt(addrArr[0])
		wArr := strings.Split(addrArr[1], "-") // eg: 1-ip:port
		if len(wArr) != 2 {
			err = fmt.Errorf("data:\"%s\" format error", data)
			res = nil
			return
		}
		var wAddr int
		wAddr, err = strconv.Atoi(wArr[0])
		if err != nil {
			err = fmt.Errorf("data:\"%s\" format error(%v)", data, err)
			return
		}
		client := &RPCClient{Addr: wArr[1], Weight: wAddr}
		val, ok := res[proto]
		if ok {
			val = append(val, client)
		} else {
			val = []*RPCClient{client}
		}
		res[proto] = val
	}
	return
}

// cometProtoInt get the figure corresponding with protocol string
func cometProtoInt(protocol string) int {
	if protocol == cometProtocolWSStr {
		return cometProtocolWS
	} else if protocol == cometProtocolTCPStr {
		return cometProtocolTCP
	} else if protocol == cometProtocolRPCStr {
		return cometProtocolRPC
	} else {
		return cometProtocolUnknown
	}
}

// GetComet get the node infomation under the node.
func GetComet(key string) *CometNodeInfo {
	if cometRing == nil || len(cometNodeInfoMap) == 0 {
		return nil
	}
	node := cometRing.Hash(key)
	glog.V(1).Infof("cometHash hits \"%s\"", node)
	return cometNodeInfoMap[node]
}

// InitComet init a rand lb rpc for comet module.
func InitComet(conn *zk.Conn, fpath string, retry, ping time.Duration, vnode int) {
	// watch comet path
	ch := make(chan *CometNodeEvent, 1024)
	go handleCometNodeEvent(conn, fpath, retry, ping, vnode, ch)
	go watchCometRoot(conn, fpath, ch)
}
