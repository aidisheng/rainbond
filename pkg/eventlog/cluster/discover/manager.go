// RAINBOND, Application Management Platform
// Copyright (C) 2014-2017 Goodrain Co., Ltd.

// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version. For any non-GPL usage of Rainbond,
// one or multiple Commercial Licenses authorized by Goodrain Co., Ltd.
// must be obtained first.

// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.

// You should have received a copy of the GNU General Public License
// along with this program. If not, see <http://www.gnu.org/licenses/>.

package discover

import (
	"net"
	"os"
	"strings"
	"time"

	"github.com/goodrain/rainbond/pkg/eventlog/conf"
	"github.com/goodrain/rainbond/pkg/eventlog/util"

	"fmt"

	"encoding/json"

	"github.com/Sirupsen/logrus"
	"github.com/coreos/etcd/client"
	"github.com/coreos/etcd/clientv3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/twinj/uuid"
	"golang.org/x/net/context"
)

//Manager 节点动态发现管理器
type Manager interface {
	RegisteredInstance(host string, port int, stopRegister *bool) *Instance
	CancellationInstance(instance *Instance)
	MonitorAddInstances() chan *Instance
	MonitorDelInstances() chan *Instance
	MonitorUpdateInstances() chan *Instance
	GetInstance(string) *Instance
	InstanceCheckHealth(string) string
	Run() error
	GetCurrentInstance() Instance
	Stop()
	Scrape(ch chan<- prometheus.Metric, namespace, exporter string) error
}

//EtcdDiscoverManager 基于ETCD自动发现
type EtcdDiscoverManager struct {
	cancel         func()
	context        context.Context
	addChan        chan *Instance
	delChan        chan *Instance
	updateChan     chan *Instance
	log            *logrus.Entry
	conf           conf.DiscoverConf
	etcdAPI        client.KeysAPI
	etcdclientv3   *clientv3.Client
	selfInstance   *Instance
	othersInstance []*Instance
	stopDiscover   bool
}

//New 创建
func New(conf conf.DiscoverConf, log *logrus.Entry) Manager {
	ctx, cancel := context.WithCancel(context.Background())
	return &EtcdDiscoverManager{
		conf:           conf,
		cancel:         cancel,
		context:        ctx,
		log:            log,
		addChan:        make(chan *Instance, 2),
		delChan:        make(chan *Instance, 2),
		updateChan:     make(chan *Instance, 2),
		othersInstance: make([]*Instance, 0),
	}
}

//GetCurrentInstance 获取当前节点
func (d *EtcdDiscoverManager) GetCurrentInstance() Instance {
	return *d.selfInstance
}

//RegisteredInstance 注册实例
func (d *EtcdDiscoverManager) RegisteredInstance(host string, port int, stopRegister *bool) *Instance {
	instance := &Instance{}
	for !*stopRegister {
		if host == "0.0.0.0" || host == "127.0.0.1" || host == "localhost" {
			if d.conf.InstanceIP != "" {
				ip := net.ParseIP(d.conf.InstanceIP)
				if ip != nil {
					instance.HostIP = ip
				}
			}
		} else {
			ip := net.ParseIP(host)
			if ip != nil {
				instance.HostIP = ip
			}
		}
		if instance.HostIP == nil {
			ip, err := util.ExternalIP()
			if err != nil {
				d.log.Error("Can not get host ip for the instance.")
				time.Sleep(time.Second * 10)
				continue
			} else {
				instance.HostIP = ip
			}
		}
		instance.PubPort = port
		instance.DockerLogPort = d.conf.DockerLogPort
		instance.WebPort = d.conf.WebPort
		hostID, err := util.GetHostID()
		if err != nil {
			d.log.Error("Read host id from file error.", err.Error())
			hostID = uuid.NewV4().String()
		}
		if len(hostID) < 32 {
			d.log.Error("Read host id from file error. Invalid hostID ")
			hostID = uuid.NewV4().String()
		}
		instance.HostID = hostID[len(hostID)-12:]
		instance.HostName, _ = os.Hostname()
		instance.Status = "create"
		data, err := json.Marshal(instance)
		if err != nil {
			d.log.Error("Create register instance data error.", err.Error())
			time.Sleep(time.Second * 10)
			continue
		}
		_, err = d.etcdAPI.Set(d.context, fmt.Sprintf("%s/instance/%s:%d", d.conf.HomePath, instance.HostIP, instance.PubPort), string(data), nil)
		if err != nil {
			if cerr, ok := err.(client.Error); ok {
				if cerr.Code == client.ErrorCodeNodeExist {
					goto success
				}
			}
			d.log.Error("Register instance data to etcd error.", err.Error())
			time.Sleep(time.Second * 10)
			continue
		}
	success:
		d.selfInstance = instance
		go d.discover()
		d.log.Infof("Register instance in cluster success. HostID:%s HostIP:%s PubPort:%d", instance.HostID, instance.HostIP, instance.PubPort)
		return instance
	}
	return nil
}

//MonitorAddInstances 实例通知
func (d *EtcdDiscoverManager) MonitorAddInstances() chan *Instance {
	return d.addChan
}

//MonitorDelInstances 实例通知
func (d *EtcdDiscoverManager) MonitorDelInstances() chan *Instance {
	return d.delChan
}

//MonitorUpdateInstances 实例通知
func (d *EtcdDiscoverManager) MonitorUpdateInstances() chan *Instance {
	return d.updateChan
}

//Run 启动
func (d *EtcdDiscoverManager) Run() error {
	d.log.Info("Discover manager start ")
	api, err := CreateETCDClient(d.conf)
	if err != nil {
		d.log.Error("Create etcd client error.", err.Error())
		return err
	}
	d.etcdAPI = api
	d.etcdclientv3, err = clientv3.New(clientv3.Config{
		Endpoints: d.conf.EtcdAddr,
	})
	if err != nil {
		d.log.Error("Create etcd v3 client error.", err.Error())
		return err
	}
	_, err = api.Get(d.context, d.conf.HomePath+"/instance", nil)
	if err != nil {
		if client.IsKeyNotFound(err) {
			_, err = api.Set(d.context, d.conf.HomePath+"/instance", "", &client.SetOptions{Dir: true})
			if err != nil {
				if cerr, ok := err.(client.Error); ok {
					if cerr.Code != client.ErrorCodeNodeExist {
						d.log.Errorf("Create dir key `%s/instance/` to etcd error.%s", d.conf.HomePath, cerr.Message)
						return err
					}
				} else {
					d.log.Errorf("Create dir key `%s/instance/` to etcd error. %s", d.conf.HomePath, err.Error())
					return err
				}
			}
		} else {
			d.log.Errorf("Can't get `%s/instance` status. %s", d.conf.HomePath, err.Error())
			return err
		}
	}
	return nil
}

//Discover 发现
func (d *EtcdDiscoverManager) discover() {
	tike := time.NewTicker(time.Second * 5)
	defer tike.Stop()
	for {
		res, err := d.etcdAPI.Get(d.context, d.conf.HomePath+"/instance", &client.GetOptions{Recursive: true})
		if err != nil {
			d.log.Error("Get instance info from etcd error.", err.Error())
		} else {
			for _, node := range res.Node.Nodes {
				d.add(node)
			}
			break
		}
		select {
		case <-tike.C:
		case <-d.context.Done():
			return
		}
	}
	watcher := d.etcdAPI.Watcher(d.conf.HomePath+"/instance", &client.WatcherOptions{Recursive: true})
	for !d.stopDiscover {
		res, err := watcher.Next(d.context)
		if err != nil {
			if err.Error() != "context canceled" {
				d.log.Error("Watcher instance change error.", err.Error())
				select {
				case <-tike.C:
				case <-d.context.Done():
					return
				}
			}
		} else {
			switch res.Action {
			case "set":
				d.add(res.Node)
			case "delete":
				node := res.Node
				//忽略自己
				if strings.HasSuffix(node.Key, fmt.Sprintf("/%s:%d", d.selfInstance.HostIP, d.selfInstance.PubPort)) {
					continue
				}
				keys := strings.Split(node.Key, "/")
				hostPort := keys[len(keys)-1]
				d.log.Infof("Delete an instance.%s", hostPort)
				var removeIndex int
				var have bool
				for i, ins := range d.othersInstance {
					if fmt.Sprintf("%s:%d", ins.HostIP, ins.PubPort) == hostPort {
						removeIndex = i
						have = true
						break
					}
				}
				if have {
					instance := d.othersInstance[removeIndex]
					d.othersInstance = DeleteSlice(d.othersInstance, removeIndex)
					d.MonitorDelInstances() <- instance
					d.log.Infof("A instance offline %s", instance.HostName)
				}
			case "update":
				node := res.Node
				res, err := d.etcdAPI.Get(d.context, node.Key, nil)
				if err != nil {
					d.log.Error("Get instance info from etcd error.", err.Error())
				}
				d.update(res.Node)
			case "create":
				//d.log.Debug("etcd create:", res.Node)
				d.add(res.Node)
			}
		}
	}
	d.log.Debug("discover manager discover core stop")
}

func (d *EtcdDiscoverManager) add(node *client.Node) {

	//忽略自己
	if strings.HasSuffix(node.Key, fmt.Sprintf("/%s:%d", d.selfInstance.HostIP, d.selfInstance.PubPort)) {
		return
	}
	var instance Instance
	if err := json.Unmarshal([]byte(node.Value), &instance); err != nil {
		d.log.Error("Unmarshal instance data that from etcd error.", err.Error())
	} else {
		d.log.Infof("Find an instance.IP:%s, Port:%d, HostName:%s HostID: %s", instance.HostIP.String(), instance.PubPort, instance.HostName, instance.HostID)
		d.MonitorAddInstances() <- &instance
		d.othersInstance = append(d.othersInstance, &instance)
	}

}

func (d *EtcdDiscoverManager) update(node *client.Node) {

	var instance Instance
	if err := json.Unmarshal([]byte(node.Value), &instance); err != nil {
		d.log.Error("Unmarshal instance data that from etcd error.", err.Error())
	} else {
		if strings.HasSuffix(node.Key, fmt.Sprintf("/%s:%d", d.selfInstance.HostIP, d.selfInstance.PubPort)) {
			d.selfInstance = &instance
		}
		for _, i := range d.othersInstance {
			if i.HostID == instance.HostID {
				*i = instance
				d.log.Debug("update the instance " + i.HostID)
			}
		}
	}

}

//DeleteSlice 从数组中删除某元素
func DeleteSlice(source []*Instance, index int) []*Instance {
	if len(source) == 1 {
		return make([]*Instance, 0)
	}
	if index == 0 {
		return source[1:]
	}
	if index == len(source)-1 {
		return source[:len(source)-2]
	}
	return append(source[0:index-1], source[index+1:]...)
}

//Stop 停止
func (d *EtcdDiscoverManager) Stop() {
	d.stopDiscover = true
	d.cancel()
	d.log.Info("Stop the discover manager.")
}

//CancellationInstance 注销实例
func (d *EtcdDiscoverManager) CancellationInstance(instance *Instance) {
	_, err := d.etcdAPI.Delete(d.context, fmt.Sprintf("%s/instance/%s:%d", d.conf.HomePath, instance.HostIP, instance.PubPort), nil)
	if err != nil && !client.IsKeyNotFound(err) {
		d.log.Error("Cancellation Instance from etcd error.", err.Error())
	} else {
		d.log.Info("Cancellation Instance from etcd")
	}
	ctx, cancel := context.WithTimeout(d.context, time.Second*5)
	defer cancel()
	_, err = d.etcdclientv3.Delete(ctx, fmt.Sprintf("/traefik/backends/event_log_event_grpc/servers/%s/url", instance.HostID))
	if err != nil {
		d.log.Error("Cancellation Instance from etcdv3 error.", err.Error())
	}
	_, err = d.etcdclientv3.Delete(ctx, fmt.Sprintf("/traefik/backends/event_log_event_http/servers/%s/url", instance.HostID))
	if err != nil {
		d.log.Error("Cancellation Instance from etcdv3 error.", err.Error())
	}
}

//UpdateInstance 更新实例
func (d *EtcdDiscoverManager) UpdateInstance(instance *Instance) {
	instance.Status = "update"
	data, err := json.Marshal(instance)
	if err != nil {
		d.log.Error("Create update instance data error.", err.Error())
		return
	}
	_, err = d.etcdAPI.Update(d.context, fmt.Sprintf("%s/instance/%s:%d", d.conf.HomePath, instance.HostIP, instance.PubPort), string(data))
	if err != nil && !client.IsKeyNotFound(err) {
		d.log.Error(" Update Instance from etcd error.", err.Error())
	}
}

//InstanceCheckHealth 将由distribution调用，当发现节点不正常时
//此处检查，如果节点已经下线，返回 delete
//如果节点未下线标记为异常,返回 abnormal
//如果节点被集群判断为故障,返回 delete
func (d *EtcdDiscoverManager) InstanceCheckHealth(instanceID string) string {
	d.log.Info("Start check instance health.")
	if d.selfInstance.HostID == instanceID {
		d.log.Error("The current node condition monitoring.")
		return "abnormal"
	}
	for _, i := range d.othersInstance {
		if i.HostID == instanceID {
			d.log.Errorf("Instance (%s) is abnormal.", instanceID)
			i.Status = "abnormal"
			i.TagNumber++
			if i.TagNumber > ((len(d.othersInstance) + 1) / 2) { //大于一半的节点标记
				d.log.Warn("Instance (%s) is abnormal. tag number more than half of all instance number. will cancellation.", instanceID)
				d.CancellationInstance(i)
				return "delete"
			}
			d.UpdateInstance(i)
			return "abnormal"
		}
	}
	return "delete"
}

//GetInstance 获取实例
func (d *EtcdDiscoverManager) GetInstance(id string) *Instance {
	if id == d.selfInstance.HostID {
		return d.selfInstance
	}
	for _, i := range d.othersInstance {
		if i.HostID == id {
			return i
		}
	}
	return nil
}

//Scrape prometheus monitor metrics
func (d *EtcdDiscoverManager) Scrape(ch chan<- prometheus.Metric, namespace, exporter string) error {
	instanceDesc := prometheus.NewDesc(
		prometheus.BuildFQName(namespace, exporter, "instanse_up"),
		"the instance in cluster status.",
		[]string{"from", "instance", "status"}, nil,
	)
	// if d.selfInstance.Status == "abnormal" || d.selfInstance.Status == "delete" {
	// 	ch <- prometheus.MustNewConstMetric(instanceDesc, prometheus.GaugeValue, 0, d.selfInstance.HostIP.String(), d.selfInstance.HostIP.String(), d.selfInstance.Status)
	// } else {
	// 	ch <- prometheus.MustNewConstMetric(instanceDesc, prometheus.GaugeValue, 1, d.selfInstance.HostIP.String(), d.selfInstance.HostIP.String(), d.selfInstance.Status)
	// }
	for _, i := range d.othersInstance {
		if i.Status == "delete" || i.Status == "abnormal" {
			ch <- prometheus.MustNewConstMetric(instanceDesc, prometheus.GaugeValue, 0, d.selfInstance.HostIP.String(), i.HostIP.String(), i.Status)
		} else {
			ch <- prometheus.MustNewConstMetric(instanceDesc, prometheus.GaugeValue, 1, d.selfInstance.HostIP.String(), i.HostIP.String(), i.Status)
		}
	}
	return nil
}
