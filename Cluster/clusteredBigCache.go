package clusteredBigCache

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"

	"github.com/oaStuff/clusteredBigCache/bigcache"
	"github.com/oaStuff/clusteredBigCache/comms"
	"github.com/oaStuff/clusteredBigCache/message"
	"github.com/oaStuff/clusteredBigCache/utils"
	"time"
)

//Cluster configuration
type ClusteredBigCacheConfig struct {
	Id             string   `json:"id"`
	Join           bool     `json:"join"`
	JoinIp         string   `json:"join_ip"`
	LocalAddresses []string `json:"local_addresses"`
	LocalPort      int      `json:"local_port"`
	BindAll        bool     `json:"bind_all"`
	ConnectRetries int      `json:"connect_retries"`
	TerminateOnListenerExit	bool 	`json:"terminate_on_listener_exit"`
	ReplicationFactor int `json:"replication_factor"`
	WriteAck          bool   `json:"write_ack"`
}

//Cluster definition
type ClusteredBigCache struct {
	config         *ClusteredBigCacheConfig
	cache          *bigcache.BigCache
	remoteNodes    *utils.SliceList
	logger         utils.AppLogger
	lock           sync.Mutex
	serverEndpoint net.Listener
	joinQueue      chan *message.ProposedPeer
	pendingConn    sync.Map
}

//create a new local node
func New(config *ClusteredBigCacheConfig, logger utils.AppLogger) *ClusteredBigCache {

	cache, err := bigcache.NewBigCache(bigcache.DefaultConfig())
	if err != nil {
		panic(err)
	}

	return &ClusteredBigCache{
		config:      config,
		cache:       cache,
		remoteNodes: utils.NewSliceList(remoteNodeEqualFunc, remoteNodeKeyFunc),
		logger:      logger,
		lock:        sync.Mutex{},
		joinQueue:   make(chan *message.ProposedPeer, 512),
		pendingConn: sync.Map{},
	}
}

func (node *ClusteredBigCache) checkConfig()  {
	if node.config.LocalPort < 1 {
		panic("Local port can not be zero.")
	}

	if node.config.ReplicationFactor < 1 {
		utils.Warn(node.logger, "Adjusting replication to 1 (no replication) because it was less than 1")
		node.config.ReplicationFactor = 1
	}
}

//start this Cluster running
func (node *ClusteredBigCache) Start() error {

	node.checkConfig()
	if "" == node.config.Id {
		node.config.Id = utils.GenerateNodeId(32)
		utils.Info(node.logger, "Cluster ID is "+node.config.Id)
	}

	if err := node.bringNodeUp(); err != nil {
		return err
	}

	go node.connectToExistingNodes()
	if true == node.config.Join { //we are to join an existing cluster
		if err := node.joinCluster(); err != nil {
			return err
		}
	}

	return nil
}

//shut down this Cluster and all terminate all connections to remoteNodes
func (node *ClusteredBigCache) ShutDown() {
	for _, v := range node.remoteNodes.Values() {
		v.(*remoteNode).shutDown()
	}

	close(node.joinQueue)
	if node.serverEndpoint != nil {
		node.serverEndpoint.Close()
	}
}

//join an existing cluster
func (node *ClusteredBigCache) joinCluster() error {
	if "" == node.config.JoinIp {
		utils.Critical(node.logger, "the server's IP to join can not be empty.")
		return errors.New("the server's IP to join can not be empty since Join is true, there must be a JoinIP")
	}

	remoteNode := newRemoteNode(&remoteNodeConfig{IpAddress: node.config.JoinIp,
												ConnectRetries: node.config.ConnectRetries,
												Sync: true}, node, node.logger)
	remoteNode.join()

	return nil
}

//bring up this Cluster
func (node *ClusteredBigCache) bringNodeUp() error {

	var err error
	utils.Info(node.logger, "bringing up node "+node.config.Id)
	node.serverEndpoint, err = net.Listen("tcp", ":"+strconv.Itoa(node.config.LocalPort))
	if err != nil {
		utils.Error(node.logger, fmt.Sprintf("unable to Listen on port %d. [%s]", node.config.LocalPort, err.Error()))
		return err
	}

	go node.listen()
	return nil
}

//event function used by remoteNode to announce the disconnection of itself
func (node *ClusteredBigCache) eventRemoteNodeDisconneced(remoteNode *remoteNode) {

	if remoteNode.indexInParent < 0 {
		return
	}

	node.lock.Lock()
	defer node.lock.Unlock()

	node.remoteNodes.Remove(remoteNode.indexInParent)
}

//util function to return all know remoteNodes
func (node *ClusteredBigCache) getRemoteNodes() []interface{} {
	node.lock.Lock()
	defer node.lock.Unlock()

	return node.remoteNodes.Values()
}

//event function used by remoteNode to verify itself
func (node *ClusteredBigCache) eventVerifyRemoteNode(remoteNode *remoteNode) bool {
	node.lock.Lock()
	defer node.lock.Unlock()

	if node.remoteNodes.Contains(remoteNode) {
		return false
	}

	index := node.remoteNodes.Add(remoteNode)
	remoteNode.indexInParent = index
	utils.Info(node.logger, fmt.Sprintf("added remote node '%s' into group at index %d", remoteNode.config.Id, index))
	node.pendingConn.Delete(remoteNode.config.Id)

	return true
}

//event function used by remoteNode to notify this node of a connection that failed
func (node *ClusteredBigCache) eventUnableToConnect(config *remoteNodeConfig) {
	node.pendingConn.Delete(config.Id)
}

//listen for new connections to this node
func (node *ClusteredBigCache) listen() {

	utils.Info(node.logger, fmt.Sprintf("node '%s' is up and running", node.config.Id))
	errCount := 0
	for {
		conn, err := node.serverEndpoint.Accept()
		if err != nil {
			utils.Error(node.logger, err.Error())
			errCount++
			if errCount >= 5 {
				break
			}
			continue
		}
		errCount = 0

		//build a new remoteNode from this new connection
		tcpConn := conn.(*net.TCPConn)
		remoteNode := newRemoteNode(&remoteNodeConfig{IpAddress: tcpConn.RemoteAddr().String(),
														ConnectRetries: node.config.ConnectRetries,
														Sync: false}, node, node.logger)
		remoteNode.setState(nodeStateHandshake)
		remoteNode.setConnection(comms.WrapConnection(tcpConn))
		utils.Info(node.logger, fmt.Sprintf("new connection from remote '%s'", tcpConn.RemoteAddr().String()))
		remoteNode.start()
	}
	utils.Critical(node.logger, "listening loop terminated unexpectedly due to too many errors")
	if node.config.TerminateOnListenerExit {
		panic("listening loop terminated unexpectedly due to too many errors")
	}
}

func (node *ClusteredBigCache) DoTest() {
	fmt.Printf("list of keys: %+v\n", node.remoteNodes.Keys())
}

//this is a goroutine that takes details from a channel and connect to them if they are not known
//when a remote system connects to this node or when this node connects to a remote system, it will query that system
//for the list of its connected nodes and pushes that list into this channel so that this node can connect forming
//a mesh network in the process
func (node *ClusteredBigCache) connectToExistingNodes() {

	for value := range node.joinQueue {
		if _, ok := node.pendingConn.Load(value.Id); ok {
			utils.Warn(node.logger, fmt.Sprintf("remote node '%s' already in connnection pending queue", value.Id))
			continue
		}
		node.lock.Lock()
		keys := node.remoteNodes.Keys()
		node.lock.Unlock()
		if _, ok := keys[value.Id]; ok {
			continue
		}

		//we are here because we don't know this remote node
		remoteNode := newRemoteNode(&remoteNodeConfig{IpAddress: value.IpAddress,
			ConnectRetries: node.config.ConnectRetries,
			Id: value.Id, Sync: false}, node, node.logger)
		remoteNode.join()
		node.pendingConn.Store(value.Id, value.IpAddress)
	}
}

func (node *ClusteredBigCache) PutData(key string, data []byte, duration time.Duration) error {
	if node.config.ReplicationFactor == 1 {
		return node.cache.Set(key, data, duration)
	}

	if node.remoteNodes.Size() < node.config.ReplicationFactor {

	}


	return nil
}

func (node *ClusteredBigCache) GetData(key string) ([]byte, error) {
	return nil, nil
}

func (node *ClusteredBigCache) DeleteData(key string) error {
	return nil
}