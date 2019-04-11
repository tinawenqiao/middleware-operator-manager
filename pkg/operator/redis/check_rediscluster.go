package redis

import (
	"fmt"
	"github.com/golang/glog"
	redistype "harmonycloud.cn/middleware-operator-manager/pkg/apis/redis/v1alpha1"
	"harmonycloud.cn/middleware-operator-manager/util"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var reg = regexp.MustCompile(`([\d.]+):6379 \((\w+)...\) -> (\d+) keys \| (\d+) slots \| (\d+) slaves`)

//检查集群状态
//redisCluster：redisCluster对象
func (rco *RedisClusterOperator) checkAndUpdateRedisClusterStatus(redisCluster *redistype.RedisCluster) error {

	if redisCluster.Status.Phase != redistype.RedisClusterScaling &&
		redisCluster.Status.Phase != redistype.RedisClusterUpgrading &&
		redisCluster.Status.Phase != redistype.RedisClusterCreating {

		endpoints, err := rco.defaultClient.CoreV1().Endpoints(redisCluster.Namespace).Get(redisCluster.GetName(), metav1.GetOptions{})

		if err != nil {
			return fmt.Errorf("get redis cluster endpoint is error: %v", err)
		}

		if len(endpoints.Subsets) == 0 || len(endpoints.Subsets[0].Addresses) == 0 {
			return fmt.Errorf("redis cluster endpoint: %v is blank", endpoints)
		}

		clusterInstanceIp := endpoints.Subsets[0].Addresses[0].IP
		reference := endpoints.Subsets[0].Addresses[0].TargetRef

		clusterStatusIsOK, _, err := rco.execClusterInfo(clusterInstanceIp, reference.Name, reference.Namespace)
		if err != nil {
			rco.updateRedisClusterStatus(redisCluster, endpoints, redistype.RedisClusterFailed, err.Error())
			return err
		}

		if !clusterStatusIsOK {
			abnormalReason := clusterStateFailed
			if redisCluster.Status.Reason != "" {
				abnormalReason = redisCluster.Status.Reason
			}
			rco.updateRedisClusterStatus(redisCluster, endpoints, redistype.RedisClusterFailed, abnormalReason)
			return nil
		}

		_, err = rco.updateRedisClusterStatus(redisCluster, endpoints, redistype.RedisClusterRunning, "")
		return err
	}

	//这里将阻塞
	endpoints, err := rco.checkPodInstanceIsReadyByEndpoint(redisCluster)

	if err != nil {
		//更新状态为Failing
		rco.updateRedisClusterStatus(redisCluster, endpoints, redistype.RedisClusterFailed, err.Error())
		return err
	}

	if len(endpoints.Subsets) == 0 || len(endpoints.Subsets[0].Addresses) == 0 {
		return fmt.Errorf("endpoints.Subsets is nil, maybe statefulset %v/%v has been deleted", redisCluster.Namespace, redisCluster.Name)
	}

	//TODO 待优化、重构这块代码
	addresses := endpoints.Subsets[0].Addresses
	//异常场景处理
	switch redisCluster.Status.Phase {
	case redistype.RedisClusterCreating:
		var isClusteredAddress, isOnlyKnownSelfAddress []v1.EndpointAddress
		for _, addr := range addresses {
			isOK, isOnlyKnownSelf, err := rco.execClusterInfo(addr.IP, addr.TargetRef.Name, addr.TargetRef.Namespace)
			if err != nil {
				return err
			}

			//如果isOnlyKnownSelf为false,其状态为fail,表示集群有问题(可能原因pod实例IP和node配置里的IP不匹配)
			if !isOnlyKnownSelf && !isOK {
				glog.Errorf("%v by instance: %v", clusterStateFailed, addr.IP)
				rco.updateRedisClusterStatus(redisCluster, endpoints, redistype.RedisClusterFailed, clusterStateFailed)
				return nil
			}

			//该addr对应的podIP已经组成集群
			if !isOnlyKnownSelf && isOK {
				isClusteredAddress = append(isClusteredAddress, addr)
			} else {
				//不会出现isOnlyKnownSelf和isOK同时为true
				//独立的实例,没有加入到集群
				isOnlyKnownSelfAddress = append(isOnlyKnownSelfAddress, addr)
			}
		}

		//表示新集群,所有实例都没有初始化,开始初始化集群
		if int32(len(isOnlyKnownSelfAddress)) == *redisCluster.Spec.Replicas || len(isClusteredAddress) == 0 {
			return rco.createAndInitRedisCluster(redisCluster)
		}

		//查询当前集群的节点信息
		nodeInfos, err := rco.execClusterNodes(isClusteredAddress[0].IP, isClusteredAddress[0].TargetRef.Namespace, isClusteredAddress[0].TargetRef.Name)
		if err != nil {
			//更新状态为Failed
			rco.updateRedisClusterStatus(redisCluster, endpoints, redistype.RedisClusterFailed, err.Error())
			return err
		}

		var existedMasterInstanceIPs, existedSlaveInstanceIPs []string
		masterSlaveConnector := make(map[string]string, len(addresses))
		for _, info := range nodeInfos {
			if masterFlagType == info.Flags {
				masterIP := strings.Split(info.IpPort, ":")[0]
				for _, info1 := range nodeInfos {
					if info.NodeId == info1.Master {
						masterSlaveConnector[masterIP] = strings.Split(info1.IpPort, ":")[0]
						break
					}
				}
				existedMasterInstanceIPs = append(existedMasterInstanceIPs, masterIP)
			} else {
				existedSlaveInstanceIPs = append(existedSlaveInstanceIPs, strings.Split(info.IpPort, ":")[0])
			}
		}

		var existInstanceIp string
		if len(existedMasterInstanceIPs) != 0 {
			existInstanceIp = existedMasterInstanceIPs[0]
		} else if len(existedSlaveInstanceIPs) != 0 {
			existInstanceIp = existedSlaveInstanceIPs[0]
		} else {
			err := fmt.Errorf("existedMasterInstanceIPs and existedSlaveInstanceIPs is all blank")
			glog.Error(err.Error())
			//更新状态为Failed
			rco.updateRedisClusterStatus(redisCluster, endpoints, redistype.RedisClusterFailed, err.Error())
			return err
		}

		masterInstanceIPs, slaveInstanceIPs, err := rco.waitExpectMasterSlaveIPAssign(addresses, masterSlaveConnector)

		//收集待加入集群的master节点
		var willAddClusterMasterIps []string
		for _, masterIPs := range masterInstanceIPs {
			if !util.InSlice(masterIPs, existedMasterInstanceIPs) {
				willAddClusterMasterIps = append(willAddClusterMasterIps, masterIPs)
			}
		}

		reference := addresses[0].TargetRef
		//加master
		if len(willAddClusterMasterIps) != 0 {
			err = rco.addMasterNodeToRedisCluster(willAddClusterMasterIps, existInstanceIp, reference.Name, reference.Namespace)
			if err != nil {
				//更新状态为Failed
				rco.updateRedisClusterStatus(redisCluster, endpoints, redistype.RedisClusterFailed, err.Error())
				return err
			}
		}

		//查询新建集群的节点信息,主要是master的nodeId信息
		nodeInfos, err = rco.execClusterNodes(existInstanceIp, reference.Namespace, reference.Name)

		if err != nil {
			//更新状态为Failed
			rco.updateRedisClusterStatus(redisCluster, endpoints, redistype.RedisClusterFailed, err.Error())
			return err
		}

		//收集待加入集群的slave节点以及slave对应的master ips
		var willAddClusterSlaveIps []string
		var slaveParentIps []string
		for i, slaveIPs := range slaveInstanceIPs {
			if !util.InSlice(slaveIPs, existedSlaveInstanceIPs) {
				willAddClusterSlaveIps = append(willAddClusterSlaveIps, slaveIPs)
				slaveParentIps = append(slaveParentIps, masterInstanceIPs[i])
			}
		}

		if len(slaveParentIps) != 0 {
			// 根据slaveParentIps拿nodeId,下标对应
			var masterInstanceNodeIds []string
			for _, masterInstanceIp := range slaveParentIps {
				for _, info := range nodeInfos {
					if masterInstanceIp+":6379" == info.IpPort {
						masterInstanceNodeIds = append(masterInstanceNodeIds, info.NodeId)
						break
					}
				}
			}

			err = rco.addSlaveToClusterMaster(willAddClusterSlaveIps, slaveParentIps, masterInstanceNodeIds, reference.Namespace, reference.Name)
			if err != nil {
				//更新状态为Failed
				rco.updateRedisClusterStatus(redisCluster, endpoints, redistype.RedisClusterFailed, err.Error())
				return err
			}
		}

		err = rco.rebalanceRedisClusterSlotsToMasterNode(existInstanceIp, reference.Name, reference.Namespace, "")
		if err != nil {
			//更新状态为Failed
			rco.updateRedisClusterStatus(redisCluster, endpoints, redistype.RedisClusterFailed, err.Error())
			return err
		}

		glog.Infof("cluster fix create and init success")
		//更新状态为Running
		_, err = rco.updateRedisClusterStatus(redisCluster, endpoints, redistype.RedisClusterRunning, "")

		return err

	case redistype.RedisClusterScaling:
		//复制出一个Endpoints
		oldEndpoints := endpoints.DeepCopy()
		//将address根据podName从大到小排序
		sort.SliceStable(addresses, func(i, j int) bool {
			name1 := addresses[i].TargetRef.Name
			name2 := addresses[j].TargetRef.Name
			return name1 > name2
		})
		//没有scale前的endpoint里address应该是Conditions长度
		subLen := len(addresses) - len(redisCluster.Status.Conditions)
		oldEndpoints.Subsets[0].Addresses = addresses[subLen:]
		return rco.upgradeRedisCluster(redisCluster, oldEndpoints)
	case redistype.RedisClusterUpgrading:
		var isClusteredAddress, isOnlyKnownSelfAddress []v1.EndpointAddress
		for _, addr := range addresses {
			isOK, isOnlyKnownSelf, err := rco.execClusterInfo(addr.IP, addr.TargetRef.Name, addr.TargetRef.Namespace)
			if err != nil {
				return err
			}

			//如果isOnlyKnownSelf为false,其状态为fail,表示集群有问题(可能原因pod实例IP和node配置里的IP不匹配)
			if !isOnlyKnownSelf && !isOK {
				glog.Errorf("%v by instance: %v", clusterStateFailed, addr.IP)
				rco.updateRedisClusterStatus(redisCluster, endpoints, redistype.RedisClusterFailed, clusterStateFailed)
				return nil
			}

			//该addr对应的podIP已经组成集群
			if !isOnlyKnownSelf && isOK {
				isClusteredAddress = append(isClusteredAddress, addr)
			} else {
				//不会出现isOnlyKnownSelf和isOK同时为true
				//独立的实例,没有加入到集群
				isOnlyKnownSelfAddress = append(isOnlyKnownSelfAddress, addr)
			}
		}

		//复制出一个Endpoints
		oldEndpoints := endpoints.DeepCopy()
		//将address根据podName从大到小排序
		sort.SliceStable(addresses, func(i, j int) bool {
			name1 := addresses[i].TargetRef.Name
			name2 := addresses[j].TargetRef.Name
			return name1 > name2
		})
		//新增的实例数
		subLen := len(addresses) - len(redisCluster.Status.Conditions)
		oldEndpoints.Subsets[0].Addresses = addresses[subLen:]

		//没有形成集群的实例数如果等于新加实例数,则说明还没有进行升级操作
		if subLen == len(isOnlyKnownSelfAddress) {
			return rco.upgradeRedisCluster(redisCluster, oldEndpoints)
		}

		//查询当前集群的节点信息
		nodeInfos, err := rco.execClusterNodes(isClusteredAddress[0].IP, isClusteredAddress[0].TargetRef.Namespace, isClusteredAddress[0].TargetRef.Name)
		if err != nil {
			//更新状态为Running
			rco.updateRedisClusterStatus(redisCluster, endpoints, redistype.RedisClusterRunning, err.Error())
			return err
		}

		var existedMasterInstanceIPs, existedSlaveInstanceIPs []string
		masterSlaveConnector := make(map[string]string, len(addresses))
		for _, info := range nodeInfos {
			if masterFlagType == info.Flags {
				masterIP := strings.Split(info.IpPort, ":")[0]
				for _, info1 := range nodeInfos {
					if info.NodeId == info1.Master {
						masterSlaveConnector[masterIP] = strings.Split(info1.IpPort, ":")[0]
						break
					}
				}
				existedMasterInstanceIPs = append(existedMasterInstanceIPs, masterIP)
			} else {
				existedSlaveInstanceIPs = append(existedSlaveInstanceIPs, strings.Split(info.IpPort, ":")[0])
			}
		}

		var existInstanceIp string
		if len(existedMasterInstanceIPs) != 0 {
			existInstanceIp = existedMasterInstanceIPs[0]
		} else if len(existedSlaveInstanceIPs) != 0 {
			existInstanceIp = existedSlaveInstanceIPs[0]
		} else {
			err := fmt.Errorf("existedMasterInstanceIPs and existedSlaveInstanceIPs is all blank")
			glog.Error(err.Error())
			//更新状态为Running
			rco.updateRedisClusterStatus(redisCluster, endpoints, redistype.RedisClusterRunning, err.Error())
			return err
		}

		masterInstanceIPs, slaveInstanceIPs, err := rco.waitExpectMasterSlaveIPAssign(addresses[:subLen], masterSlaveConnector)

		//收集待加入集群的master节点
		var willAddClusterMasterIps []string
		for _, masterIPs := range masterInstanceIPs {
			if !util.InSlice(masterIPs, existedMasterInstanceIPs) {
				willAddClusterMasterIps = append(willAddClusterMasterIps, masterIPs)
			}
		}

		reference := addresses[0].TargetRef
		//加master
		if len(willAddClusterMasterIps) != 0 {
			err = rco.addMasterNodeToRedisCluster(willAddClusterMasterIps, existInstanceIp, reference.Name, reference.Namespace)
			if err != nil {
				//更新状态为Running
				rco.updateRedisClusterStatus(redisCluster, endpoints, redistype.RedisClusterRunning, err.Error())
				return err
			}
		}

		//查询新建集群的节点信息,主要是master的nodeId信息
		nodeInfos, err = rco.execClusterNodes(existInstanceIp, reference.Namespace, reference.Name)

		if err != nil {
			//更新状态为Failed
			rco.updateRedisClusterStatus(redisCluster, endpoints, redistype.RedisClusterRunning, err.Error())
			return err
		}

		//收集待加入集群的slave节点以及slave对应的master ips
		var willAddClusterSlaveIps []string
		var slaveParentIps []string
		for i, slaveIPs := range slaveInstanceIPs {
			if !util.InSlice(slaveIPs, existedSlaveInstanceIPs) {
				willAddClusterSlaveIps = append(willAddClusterSlaveIps, slaveIPs)
				slaveParentIps = append(slaveParentIps, masterInstanceIPs[i])
			}
		}

		if len(slaveParentIps) != 0 {
			// 根据slaveParentIps拿nodeId,下标对应
			var masterInstanceNodeIds []string
			for _, masterInstanceIp := range slaveParentIps {
				for _, info := range nodeInfos {
					if masterInstanceIp+":6379" == info.IpPort {
						masterInstanceNodeIds = append(masterInstanceNodeIds, info.NodeId)
						break
					}
				}
			}

			err = rco.addSlaveToClusterMaster(willAddClusterSlaveIps, slaveParentIps, masterInstanceNodeIds, reference.Namespace, reference.Name)
			if err != nil {
				//更新状态为Running
				rco.updateRedisClusterStatus(redisCluster, endpoints, redistype.RedisClusterRunning, err.Error())
				return err
			}
		}

		upgradeType := redisCluster.Spec.UpdateStrategy.Type
		switch upgradeType {
		case redistype.AutoReceiveStrategyType:
			err = rco.rebalanceRedisClusterSlotsToMasterNode(existInstanceIp, reference.Name, reference.Namespace, redisCluster.Spec.UpdateStrategy.Pipeline)
			if err != nil {
				//更新状态为Running
				rco.updateRedisClusterStatus(redisCluster, endpoints, redistype.RedisClusterRunning, err.Error())
				return err
			}
		case redistype.AssignReceiveStrategyType:
			infos, err := rco.execRedisTribInfo(existInstanceIp, reference.Name, reference.Namespace)
			if err != nil {
				//更新状态为Running
				rco.updateRedisClusterStatus(redisCluster, endpoints, redistype.RedisClusterRunning, err.Error())
				return err
			}

			//查看卡槽分配情况
			var willAssginSlotsIP []string
			for _, info := range infos {
				if info.Slots == 0 {
					willAssginSlotsIP = append(willAssginSlotsIP, info.Ip)
				}
			}

			if len(willAssginSlotsIP) != 0 {
				updateStrategy := redisCluster.Spec.UpdateStrategy.DeepCopy()
				strategies := updateStrategy.AssignStrategies
				strategyLen := len(strategies)
				willAssignIpLen := len(willAssginSlotsIP)
				if strategyLen < willAssignIpLen {
					err := fmt.Errorf("Assign slots to new master strategies is error: strategyCount too less")
					glog.Error(err.Error())
					//更新状态为Running
					rco.updateRedisClusterStatus(redisCluster, endpoints, redistype.RedisClusterRunning, err.Error())
					return err
				}

				updateStrategy.AssignStrategies = strategies[(strategyLen - willAssignIpLen):]

				//查询新建集群的节点信息,主要是master的nodeId信息
				nodeInfos, err = rco.execClusterNodes(existInstanceIp, reference.Namespace, reference.Name)

				if err != nil {
					//更新状态为Failed
					rco.updateRedisClusterStatus(redisCluster, endpoints, redistype.RedisClusterRunning, err.Error())
					return err
				}

				var willAssginSlotsNodeIds []string
				for _, willIp := range willAssginSlotsIP {
					for _, info := range nodeInfos {
						if willIp+":6379" == info.IpPort {
							willAssginSlotsNodeIds = append(willAssginSlotsNodeIds, info.NodeId)
						}
					}
				}

				err = rco.reshareRedisClusterSlotsToMasterNode(*updateStrategy, existInstanceIp, reference.Name, reference.Namespace, willAssginSlotsNodeIds)
				if err != nil {
					//更新状态为Running
					rco.updateRedisClusterStatus(redisCluster, endpoints, redistype.RedisClusterRunning, err.Error())
					return err
				}
			}

		default:
			err := fmt.Errorf("redisCluster UpdateStrategy Type only [AutoReceive, AssignReceive] error")
			glog.Error(err.Error())
			return err
		}

		glog.Infof("cluster fix upgrade success")
		//更新状态为Running
		_, err = rco.updateRedisClusterStatus(redisCluster, endpoints, redistype.RedisClusterRunning, "")

		return err
	}

	return nil
}

//查看redis集群master信息
//existInstanceIp：redis集群中的一个实例IP
//podName：pod实例中的一个podName,不一定已经加入redis集群
//namespace：redis集群所在的ns
func (rco *RedisClusterOperator) execRedisTribInfo(existInstanceIp, podName, namespace string) ([]redisTribInfo, error) {
	infoCmd := fmt.Sprintf(" redis-trib.rb info %v:6379 ", existInstanceIp)
	commandInfo := []string{"/bin/sh", "-c", infoCmd}
	stdout, stderr, err := rco.ExecToPodThroughAPI(commandInfo, "", podName, namespace, nil)

	if err != nil || stderr != "" || strings.Contains(stdout, "[ERR]") {
		err := fmt.Errorf("redisClusterInstance: %v/%v -- infoCmd: %v\n -- stdout: %v\n -- stderr: %v\n -- error: %v", namespace, podName, commandInfo, stdout, stderr, err)
		glog.Errorf(err.Error())
		return []redisTribInfo{}, err
	}
	glog.V(4).Infof("redisClusterInstance: %v/%v infoCmd: %v \n -- \nstdout: %v", namespace, podName, commandInfo, stdout)

	var masterInfos []redisTribInfo
	infos := strings.Split(stdout, "\n")
	for _, info := range infos {
		submatches := reg.FindStringSubmatch(info)
		if len(submatches) != 6 {
			continue
		}
		keys, _ := strconv.Atoi(submatches[3])
		slots, _ := strconv.Atoi(submatches[4])
		slaves, _ := strconv.Atoi(submatches[5])
		masterInfo := redisTribInfo{
			Ip:           submatches[1],
			Port:         strconv.Itoa(redisServicePort6379),
			NodeIdPrefix: submatches[2],
			Keys:         keys,
			Slots:        slots,
			Slaves:       slaves,
		}
		masterInfos = append(masterInfos, masterInfo)
	}

	return masterInfos, nil
}
