package client

import (
	"bufio"
	"context"
	"errors"
	log2 "github.com/smallnest/rpcx/log"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/juju/ratelimit"
	ex "github.com/smallnest/rpcx/errors"
	"github.com/smallnest/rpcx/protocol"
	"github.com/smallnest/rpcx/serverplugin"
	"github.com/smallnest/rpcx/share"
	//. "github.com/halokid/ColorfulRabbit"

	"github.com/halokid/ColorfulRabbit"
	"github.com/mozillazg/request"
)

const (
	FileTransferBufferSize = 1024
)

var (
	// ErrXClientShutdown xclient is shutdown.
	ErrXClientShutdown = errors.New("xClient is shut down")
	// ErrXClientNoServer selector can't found one server.
	ErrXClientNoServer = errors.New("服务不可用或没有相应的服务名----can not found any server")
	// ErrServerUnavailable selected server is unavailable.
	ErrServerUnavailable = errors.New("selected server is unavilable")
)

// XClient is an interface that used by client with service discovery and service governance.
// One XClient is used only for one service. You should create multiple XClient for multiple services.
type XClient interface {
	SetPlugins(plugins PluginContainer)
	GetPlugins() PluginContainer
	SetSelector(s Selector)
	ConfigGeoSelector(latitude, longitude float64)
	Auth(auth string)

	Go(ctx context.Context, serviceMethod string, args interface{}, reply interface{}, done chan *Call) (*Call, error)
	Call(ctx context.Context, serviceMethod string, args interface{}, reply interface{}) error
  CallNotGo(svc string, md string, pairs []*KVPair) string
	Broadcast(ctx context.Context, serviceMethod string, args interface{}, reply interface{}) error
	Fork(ctx context.Context, serviceMethod string, args interface{}, reply interface{}) error
	SendRaw(ctx context.Context, r *protocol.Message) (map[string]string, []byte, error)
	SendFile(ctx context.Context, fileName string, rateInBytesPerSecond int64) error
	DownloadFile(ctx context.Context, requestFileName string, saveTo io.Writer) error
	Close() error

	// 非go语言
	IsGo() bool
	GetNotGoServers() map[string]string
}

// KVPair contains a key and a string.
type KVPair struct {
	Key   string
	Value string
}

// ServiceDiscoveryFilter can be used to filter services with customized logics.
// Servers can register its services but clients can use the customized filter to select some services.
// It returns true if ServiceDiscovery wants to use this service, otherwise it returns false.
type ServiceDiscoveryFilter func(kvp *KVPair) bool

// ServiceDiscovery defines ServiceDiscovery of zookeeper, etcd and consul
type ServiceDiscovery interface {
	// todo: 每种服务注册的方式都要继续这个interface的方法
	GetServices() []*KVPair
	WatchService() chan []*KVPair
	RemoveWatcher(ch chan []*KVPair)
	Clone(servicePath string) ServiceDiscovery
	SetFilter(ServiceDiscoveryFilter)
	Close()
}

type xClient struct {
	failMode     FailMode
	selectMode   SelectMode
	cachedClient map[string]RPCClient
	breakers     sync.Map
	servicePath  string
	option       Option

	mu        sync.RWMutex
	servers   map[string]string
	discovery ServiceDiscovery
	selector  Selector

	isShutdown bool

	// auth is a string for Authentication, for example, "Bearer mF_9.B5f-4.1JqM"
	auth string

	Plugins PluginContainer

	ch chan []*KVPair

	serverMessageChan chan<- *protocol.Message
	
	isGo bool			// 非go服务端的标识
	svcTyp   string
	notGoServers map[string]string			// 非go服务端的地址
}

// NewXClient creates a XClient that supports service discovery and service governance.
func NewXClient(servicePath string, failMode FailMode, selectMode SelectMode, discovery ServiceDiscovery, option Option) XClient {
	client := &xClient{
		failMode:     failMode,
		selectMode:   selectMode,
		discovery:    discovery,
		servicePath:  servicePath,
		cachedClient: make(map[string]RPCClient),
		option:       option,
	}
	client.isGo = true

	pairs := discovery.GetServices()
	servers := make(map[string]string, len(pairs))
	/**
	kCk := ""
	for i, p := range pairs {
		if i == 0 {
			kCk = p.Key	
		}
		servers[p.Key] = p.Value
	}
	*/
	for _, p := range pairs {
		servers[p.Key] = p.Value
	}
	filterByStateAndGroup(client.option.Group, servers)

	// todo: 定义servers， 修复相似服务名bug在这里
	client.servers = servers
	log2.ADebug.Print("client.servers ---------------", client.servers)
	
	// 检查第一个key属于什么typ
	//serCk := servers[kCk]
	//typ := GetSpIdx(serCk, "&", -1)
	//if typ == "typ=py" {
		// python服务端
		//return "pyTyp"
	//}
	
	//log.Println("找到的servers:", servers)
	/*
	isNotGoSvc := []string{"typ=py", "typ=rust"}
	for _, v := range servers {
		//if strings.Index(v, "typ=py") != -1 {
		if ColorfulRabbit.InSlice(v, isNotGoSvc) {	
			// 为非go语言
			client.isGo = false 
			client.notGoServers = servers
			break
		} 
	}
	*/
	genNotGoSvc(client, servers)
	
	if selectMode != Closest && selectMode != SelectByUser {
		client.selector = newSelector(selectMode, servers)
	}

	client.Plugins = &pluginContainer{}

	ch := client.discovery.WatchService()
	if ch != nil {
		client.ch = ch
		go client.watch(ch)
	}

	return client
}

func genNotGoSvc(client *xClient, servers map[string]string) error {
	// generate the info for not go service
	isNotGo := false
	for _, v := range servers {
		if strings.Index(v, "typ=py") != -1 {
			//client.isGo = false
			client.svcTyp = "py"
			isNotGo = true
			//client.notGoServers = servers
			//break
		} else if strings.Index(v, "typ=rust") != -1 {
			client.svcTyp = "rust"
			isNotGo = true
		}

		if isNotGo {
			client.isGo = false
			client.notGoServers = servers
		}
		break
	}
	log.Printf("client genNotGoSvc -------------- %+v", client)
	return nil
}

// NewBidirectionalXClient creates a new xclient that can receive notifications from servers.
func NewBidirectionalXClient(servicePath string, failMode FailMode, selectMode SelectMode, discovery ServiceDiscovery, option Option, serverMessageChan chan<- *protocol.Message) XClient {
	client := &xClient{
		failMode:          failMode,
		selectMode:        selectMode,
		discovery:         discovery,
		servicePath:       servicePath,
		cachedClient:      make(map[string]RPCClient),
		option:            option,
		serverMessageChan: serverMessageChan,
	}

	pairs := discovery.GetServices()
	servers := make(map[string]string, len(pairs))
	for _, p := range pairs {
		servers[p.Key] = p.Value
	}
	filterByStateAndGroup(client.option.Group, servers)
	client.servers = servers
	if selectMode != Closest && selectMode != SelectByUser {
		client.selector = newSelector(selectMode, servers)
	}

	client.Plugins = &pluginContainer{}

	ch := client.discovery.WatchService()
	if ch != nil {
		client.ch = ch
		go client.watch(ch)
	}

	return client
}

// SetSelector sets customized selector by users.
func (c *xClient) SetSelector(s Selector) {
	c.mu.RLock()
	s.UpdateServer(c.servers)
	c.mu.RUnlock()

	c.selector = s
}

// SetPlugins sets client's plugins.
func (c *xClient) SetPlugins(plugins PluginContainer) {
	c.Plugins = plugins
}

func (c *xClient) GetPlugins() PluginContainer {
	return c.Plugins
}

// ConfigGeoSelector sets location of client's latitude and longitude,
// and use newGeoSelector.
func (c *xClient) ConfigGeoSelector(latitude, longitude float64) {
	c.selector = newGeoSelector(c.servers, latitude, longitude)
	c.selectMode = Closest
}

// Auth sets s token for Authentication.
func (c *xClient) Auth(auth string) {
	c.auth = auth
}

// watch changes of service and update cached clients.
func (c *xClient) watch(ch chan []*KVPair) {
	for pairs := range ch {
		servers := make(map[string]string, len(pairs))
		for _, p := range pairs {
			servers[p.Key] = p.Value
		}
		c.mu.Lock()
		filterByStateAndGroup(c.option.Group, servers)
		c.servers = servers

		if c.selector != nil {
			c.selector.UpdateServer(servers)
		}

		c.mu.Unlock()
	}
}
func filterByStateAndGroup(group string, servers map[string]string) {
	for k, v := range servers {
		if values, err := url.ParseQuery(v); err == nil {
			if state := values.Get("state"); state == "inactive" {
				delete(servers, k)
			}
			if group != "" && group != values.Get("group") {
				delete(servers, k)
			}
		}
	}
}

// selects a client from candidates base on c.selectMode
func (c *xClient) selectClient(ctx context.Context, servicePath, serviceMethod string, args interface{}) (string, RPCClient, error) {
	//log.Println("selectClient ---------------")
	//log.Println("servers ---------------", c.servers)
	c.mu.Lock()
	k := c.selector.Select(ctx, servicePath, serviceMethod, args)
	c.mu.Unlock()
	if k == "" {
		return "", nil, ErrXClientNoServer
	}
	client, err := c.getCachedClient(k)
	return k, client, err
}

func (c *xClient) getCachedClient(k string) (RPCClient, error) {
	// TODO: improve the lock
	//log.Println("getCachedClient -----------------")
	var client RPCClient
	var needCallPlugin bool
	c.mu.Lock()
	defer func() {
		if needCallPlugin {
			c.Plugins.DoClientConnected((client.(*Client)).Conn)
		}
	}()
	defer c.mu.Unlock()

	breaker, ok := c.breakers.Load(k)
	if ok && !breaker.(Breaker).Ready() {
		return nil, ErrBreakerOpen
	}

	client = c.cachedClient[k]
	//log.Println("c.cachedClient ----- @@@@@@@@@@@@@@---- ", c.cachedClient)
	if client != nil {
		if !client.IsClosing() && !client.IsShutdown() {
			return client, nil
		}
		delete(c.cachedClient, k)
		client.Close()
	}

	client = c.cachedClient[k]
	if client == nil || client.IsShutdown() {
		network, addr := splitNetworkAndAddress(k)
		if network == "inprocess" {
			client = InprocessClient
		} else {
			client = &Client{			// todo: client本来是一个 RPCClient类型, xClient是从这里开始转变为client struct的，所以可以调用 client.conn
				option:  c.option,
				Plugins: c.Plugins,
			}

			var breaker interface{}
			if c.option.GenBreaker != nil {
				breaker, _ = c.breakers.LoadOrStore(k, c.option.GenBreaker())
			}
			//log.Println("getCache 11111111 --------------------------")
			// todo: client连接到server， 并且把连接句柄写入conn, 这是一个长连接，cache会一直保留这个连接
			// todo:  Connect函数会触发一个input的gor, go c.input() 这一句， 这个input就是更改 client.pending[seq]， 也就是网络请求连接状态的逻辑，SendRaw 和 call 都是靠这个来改变网络请求的状态
			err := client.Connect(network, addr)
			if err != nil {
				if breaker != nil {
					breaker.(Breaker).Fail()
				}
				return nil, err
			}
			if c.Plugins != nil {
				needCallPlugin = true
			}
		}

		client.RegisterServerMessageChan(c.serverMessageChan)

		c.cachedClient[k] = client
	}

	return client, nil
}

func (c *xClient) getCachedClientWithoutLock(k string) (RPCClient, error) {
	client := c.cachedClient[k]
	if client != nil {
		if !client.IsClosing() && !client.IsShutdown() {
			return client, nil
		}
		delete(c.cachedClient, k)
		client.Close()
	}

	//double check
	client = c.cachedClient[k]
	if client == nil || client.IsShutdown() {
		network, addr := splitNetworkAndAddress(k)
		if network == "inprocess" {
			client = InprocessClient
		} else {
			client = &Client{
				option:  c.option,
				Plugins: c.Plugins,
			}
			log.Println("getCache 22222 --------------------------")
			err := client.Connect(network, addr)
			if err != nil {
				return nil, err
			}
		}

		client.RegisterServerMessageChan(c.serverMessageChan)

		c.cachedClient[k] = client
	}

	return client, nil
}

func (c *xClient) removeClient(k string, client RPCClient) {
	c.mu.Lock()
	cl := c.cachedClient[k]
	if cl == client {
		delete(c.cachedClient, k)
	}
	c.mu.Unlock()

	if client != nil {
		client.UnregisterServerMessageChan()
		client.Close()
	}
}

func splitNetworkAndAddress(server string) (string, string) {
	ss := strings.SplitN(server, "@", 2)
	if len(ss) == 1 {
		return "tcp", server
	}

	return ss[0], ss[1]
}

// Go invokes the function asynchronously. It returns the Call structure representing the invocation. The done channel will signal when the call is complete by returning the same Call object. If done is nil, Go will allocate a new channel. If non-nil, done must be buffered or Go will deliberately crash.
// It does not use FailMode.
func (c *xClient) Go(ctx context.Context, serviceMethod string, args interface{}, reply interface{}, done chan *Call) (*Call, error) {
	if c.isShutdown {
		return nil, ErrXClientShutdown
	}

	if c.auth != "" {
		metadata := ctx.Value(share.ReqMetaDataKey)
		if metadata == nil {
			metadata = map[string]string{}
			ctx = context.WithValue(ctx, share.ReqMetaDataKey, metadata)
		}
		m := metadata.(map[string]string)
		m[share.AuthKey] = c.auth
	}

	_, client, err := c.selectClient(ctx, c.servicePath, serviceMethod, args)
	if err != nil {
		return nil, err
	}
	return client.Go(ctx, c.servicePath, serviceMethod, args, reply, done), nil
}

// Call invokes the named function, waits for it to complete, and returns its error status.
// It handles errors base on FailMode.
func (c *xClient) Call(ctx context.Context, serviceMethod string, args interface{}, reply interface{}) error {
	if c.isShutdown {
		return ErrXClientShutdown
	}

	if c.auth != "" {
		metadata := ctx.Value(share.ReqMetaDataKey)
		if metadata == nil {
			metadata = map[string]string{}
			ctx = context.WithValue(ctx, share.ReqMetaDataKey, metadata)
		}
		m := metadata.(map[string]string)
		m[share.AuthKey] = c.auth
	}

	var err error
	// todo: xClient在 selectClient中内存初始化为 client
	k, client, err := c.selectClient(ctx, c.servicePath, serviceMethod, args)
	if err != nil {
		if c.failMode == Failfast {
			return err
		}
	}

	var e error
	switch c.failMode {
	case Failtry:
		retries := c.option.Retries
		for retries >= 0 {
			retries--
			log2.ADebug.Print("retries ------------ ", retries)

			if client != nil {
				err = c.wrapCall(ctx, client, serviceMethod, args, reply)
				if err == nil {
					return nil
				}
				if _, ok := err.(ServiceError); ok {
					return err
				}
			}

			if uncoverError(err) {
				c.removeClient(k, client)
			}
			client, e = c.getCachedClient(k)
		}
		if err == nil {
			err = e
		}
		return err
	case Failover:
		retries := c.option.Retries
		for retries >= 0 {
			retries--

			if client != nil {
				err = c.wrapCall(ctx, client, serviceMethod, args, reply)
				if err == nil {
					return nil
				}
				if _, ok := err.(ServiceError); ok {
					return err
				}
			}

			if uncoverError(err) {
				c.removeClient(k, client)
			}
			//select another server
			k, client, e = c.selectClient(ctx, c.servicePath, serviceMethod, args)
		}

		if err == nil {
			err = e
		}
		return err
	case Failbackup:
		ctx, cancelFn := context.WithCancel(ctx)
		defer cancelFn()
		call1 := make(chan *Call, 10)
		call2 := make(chan *Call, 10)

		var reply1, reply2 interface{}

		if reply != nil {
			reply1 = reflect.New(reflect.ValueOf(reply).Elem().Type()).Interface()
			reply2 = reflect.New(reflect.ValueOf(reply).Elem().Type()).Interface()
		}

		_, err1 := c.Go(ctx, serviceMethod, args, reply1, call1)

		t := time.NewTimer(c.option.BackupLatency)
		select {
		case <-ctx.Done(): //cancel by context
			err = ctx.Err()
			return err
		case call := <-call1:
			err = call.Error
			if err == nil && reply != nil {
				reflect.ValueOf(reply).Elem().Set(reflect.ValueOf(reply1).Elem())
			}
			return err
		case <-t.C:

		}
		_, err2 := c.Go(ctx, serviceMethod, args, reply2, call2)
		if err2 != nil {
			if uncoverError(err2) {
				c.removeClient(k, client)
			}
			err = err1
			return err
		}

		select {
		case <-ctx.Done(): //cancel by context
			err = ctx.Err()
		case call := <-call1:
			err = call.Error
			if err == nil && reply != nil && reply1 != nil {
				reflect.ValueOf(reply).Elem().Set(reflect.ValueOf(reply1).Elem())
			}
		case call := <-call2:
			err = call.Error
			if err == nil && reply != nil && reply2 != nil {
				reflect.ValueOf(reply).Elem().Set(reflect.ValueOf(reply2).Elem())
			}
		}

		return err
	default: //Failfast
		err = c.wrapCall(ctx, client, serviceMethod, args, reply)
		if err != nil {
			if uncoverError(err) {
				c.removeClient(k, client)
			}
		}

		return err
	}
}


func (c *xClient) CallNotGo(svc string, md string, pairs []*KVPair) string {
	// 调用非go服务
	if len(pairs) == 0 {
		return "Call --- 服务节点数据为空"
	}
	kv := pairs[0]				// fixme： 先选择第一个节点，还需要优化算法
	if strings.Contains(kv.Value, "typ=py") {
		addrSpl := strings.Split(kv.Key, "@")
		return callPySvc(svc, md, addrSpl[1], "{}")
	}

	return ""
}

func callPySvc(svc, md, svcAddr string, params string) string {
	// 调用py服务端, jsonrpc协议
	c := &http.Client{}
	c.Timeout = time.Duration(5 * time.Second)
	req := request.NewRequest(c)
	req.Json = map[string]interface{} {
		"jsonrpc": "2.0",
		"method": svc + "." + md,
		"params": make(map[string]interface{}),				// 空map， 表示为{}
		"id": "1",
	}
	rsp, err := req.Post("http://" + svcAddr + "/api")
	ColorfulRabbit.CheckError(err, "调用服务失败", svcAddr)
	//content, _ := rsp.Content()
	js, _ := rsp.Json()
	rspCt := string(js.Get("result").MustString())
	log.Println("reqQw rsp --------------", rsp.StatusCode, rspCt)
	return rspCt
}

func uncoverError(err error) bool {
	if _, ok := err.(ServiceError); ok {
		return false
	}

	if err == context.DeadlineExceeded {
		return false
	}

	if err == context.Canceled {
		return false
	}

	return true
}
func (c *xClient) SendRaw(ctx context.Context, r *protocol.Message) (map[string]string, []byte, error) {
	log.Println("c.servers -------------------", c.servers)

	// fixme： 性能瓶颈测试 start -----------------------------------------------
	/**
	var m map[string]string
	m = make(map[string]string)
	m["X-RPCX-MessageID"] = "2"
	m["X-RPCX-MessageStatusType"] = "Normal"
	m["X-RPCX-Meta"] = ""
	m["X-RPCX-SerializeType"] = "1"
	m["X-RPCX-ServiceMethod"] = "Say"
	m["X-RPCX-ServicePath"] = "Echo"
	m["X-RPCX-Version"] = "0"
	log.Println("m ----------------", m)
	payload := []byte("performance debug!")
	log.Println("payload ----------------", string(payload))
	return m, payload, nil
	*/
	// fixme： 性能瓶颈测试 end -----------------------------------------------

	if c.isShutdown {
		return nil, nil, ErrXClientShutdown
	}

	if c.auth != "" {
		metadata := ctx.Value(share.ReqMetaDataKey)
		if metadata == nil {
			metadata = map[string]string{}
			ctx = context.WithValue(ctx, share.ReqMetaDataKey, metadata)
		}
		m := metadata.(map[string]string)
		m[share.AuthKey] = c.auth
	}

	var err error
	//log.Println("xclient SendRow selectClient -------------")
	// todo: 根据XClient的数据来生成Client，最后的SendRaw逻辑是由Client调用的
	k, client, err := c.selectClient(ctx, r.ServicePath, r.ServiceMethod, r.Payload)
	//log.Printf("c.selectClient err ----------", err.(ServiceError), "---", err.Error())
	log.Printf("DEBUG halokid 2 ------ ")

	if err != nil {
		if c.failMode == Failfast {
			return nil, nil, err
		}

		if _, ok := err.(ServiceError); ok {
			log.Printf("DEBUG halokid 3 ------ ")
			return nil, nil, err
		}
	}

	var e error
	switch c.failMode {
	case Failtry:
		retries := c.option.Retries
		for retries >= 0 {
			retries--
			if client != nil {
				log.Printf("client 22222------ %+v", client)
				// fixme: 性能优化点
				m, payload, err := client.SendRaw(ctx, r)
				if err == nil {
					return m, payload, nil
				}
				if _, ok := err.(ServiceError); ok {
					return nil, nil, err
				}
			}

			if uncoverError(err) {
				c.removeClient(k, client)
			}
			client, e = c.getCachedClient(k)
		}

		if err == nil {
			err = e
		}
		return nil, nil, err
	case Failover:			// todo: 这个是gateway默认采用的失败方式
		log.Printf("DEBUG halokid 4 ------ ")
		retries := c.option.Retries
		log.Println("Failover模式 retries --------- ", retries)
		for retries >= 0 {
			retries--
			log.Println("Failover模式 client --------- ", client)
			if client != nil {
				//log.Printf("client Failover ----- %+v", client)
				m, payload, err := client.SendRaw(ctx, r)
				//log.Println("Failover模式 err --------- ", err.Error())
				if err == nil {
					log.Println("SendRaw ----------------", m)
					//log.Println("payload ----------------", string(payload))
					return m, payload, nil
				} else {
					log.Println("[ERROR] ----------------", err.Error())
				}
				
				if _, ok := err.(ServiceError); ok {
					return nil, nil, err
				}
			}

			if uncoverError(err) {
				c.removeClient(k, client)
			}
			//select another server
			k, client, e = c.selectClient(ctx, r.ServicePath, r.ServiceMethod, r.Payload)
		}

		if err == nil {
			err = e
		}

		log.Printf("DEBUG halokid 5 ------ ")
		return nil, nil, err

	default: //Failfast
		log.Printf("client 44444------ %+v", client)
		m, payload, err := client.SendRaw(ctx, r)

		if err != nil {
			if uncoverError(err) {
				c.removeClient(k, client)
			}
		}

		log.Printf("DEBUG halokid 1 ------ ")
		return m, payload, nil
	}
}
func (c *xClient) wrapCall(ctx context.Context, client RPCClient, serviceMethod string, args interface{}, reply interface{}) error {
	if client == nil {
		return ErrServerUnavailable
	}

	ctx = share.NewContext(ctx)
	// DoPreCall会处理一些opentracking的逻辑, 封装client plugins 的 DoPostCall 方法
	c.Plugins.DoPreCall(ctx, c.servicePath, serviceMethod, args)
	// 调用服务端
	err := client.Call(ctx, c.servicePath, serviceMethod, args, reply)
	// 封装client plugins 的 DoPostCall 方法
	c.Plugins.DoPostCall(ctx, c.servicePath, serviceMethod, args, reply, err)

	return err
}

// Broadcast sends requests to all servers and Success only when all servers return OK.
// FailMode and SelectMode are meanless for this method.
// Please set timeout to avoid hanging.
func (c *xClient) Broadcast(ctx context.Context, serviceMethod string, args interface{}, reply interface{}) error {
	if c.isShutdown {
		return ErrXClientShutdown
	}

	if c.auth != "" {
		metadata := ctx.Value(share.ReqMetaDataKey)
		if metadata == nil {
			metadata = map[string]string{}
			ctx = context.WithValue(ctx, share.ReqMetaDataKey, metadata)
		}
		m := metadata.(map[string]string)
		m[share.AuthKey] = c.auth
	}

	var clients = make(map[string]RPCClient)
	c.mu.Lock()
	for k := range c.servers {
		client, err := c.getCachedClientWithoutLock(k)
		if err != nil {
			continue
		}
		clients[k] = client
	}
	c.mu.Unlock()

	if len(clients) == 0 {
		return ErrXClientNoServer
	}

	var err = &ex.MultiError{}
	l := len(clients)
	done := make(chan bool, l)
	for k, client := range clients {
		k := k
		client := client
		go func() {
			e := c.wrapCall(ctx, client, serviceMethod, args, reply)
			done <- (e == nil)
			if e != nil {
				if uncoverError(err) {
					c.removeClient(k, client)
				}
				err.Append(e)
			}
		}()
	}

	timeout := time.After(time.Minute)
check:
	for {
		select {
		case result := <-done:
			l--
			if l == 0 || !result { // all returns or some one returns an error
				break check
			}
		case <-timeout:
			err.Append(errors.New(("timeout")))
			break check
		}
	}

	if err.Error() == "[]" {
		return nil
	}
	return err
}

// Fork sends requests to all servers and Success once one server returns OK.
// FailMode and SelectMode are meanless for this method.
func (c *xClient) Fork(ctx context.Context, serviceMethod string, args interface{}, reply interface{}) error {
	if c.isShutdown {
		return ErrXClientShutdown
	}

	if c.auth != "" {
		metadata := ctx.Value(share.ReqMetaDataKey)
		if metadata == nil {
			metadata = map[string]string{}
			ctx = context.WithValue(ctx, share.ReqMetaDataKey, metadata)
		}
		m := metadata.(map[string]string)
		m[share.AuthKey] = c.auth
	}

	var clients = make(map[string]RPCClient)
	c.mu.Lock()
	for k := range c.servers {
		client, err := c.getCachedClientWithoutLock(k)
		if err != nil {
			continue
		}
		clients[k] = client
	}
	c.mu.Unlock()

	if len(clients) == 0 {
		return ErrXClientNoServer
	}

	var err = &ex.MultiError{}
	l := len(clients)
	done := make(chan bool, l)
	for k, client := range clients {
		k := k
		client := client
		go func() {
			var clonedReply interface{}
			if reply != nil {
				clonedReply = reflect.New(reflect.ValueOf(reply).Elem().Type()).Interface()
			}

			e := c.wrapCall(ctx, client, serviceMethod, args, clonedReply)
			if e == nil && reply != nil && clonedReply != nil {
				reflect.ValueOf(reply).Elem().Set(reflect.ValueOf(clonedReply).Elem())
			}
			done <- (e == nil)
			if e != nil {
				if uncoverError(err) {
					c.removeClient(k, client)
				}
				err.Append(e)
			}

		}()
	}

	timeout := time.After(time.Minute)
check:
	for {
		select {
		case result := <-done:
			l--
			if result {
				return nil
			}
			if l == 0 { // all returns or some one returns an error
				break check
			}

		case <-timeout:
			err.Append(errors.New(("timeout")))
			break check
		}
	}

	if err.Error() == "[]" {
		return nil
	}

	return err
}

// SendFile sends a local file to the server.
// fileName is the path of local file.
// rateInBytesPerSecond can limit bandwidth of sending,  0 means does not limit the bandwidth, unit is bytes / second.
func (c *xClient) SendFile(ctx context.Context, fileName string, rateInBytesPerSecond int64) error {
	file, err := os.Open(fileName)
	if err != nil {
		return err
	}

	fi, err := os.Stat(fileName)
	if err != nil {
		return err
	}

	args := serverplugin.FileTransferArgs{
		FileName: fi.Name(),
		FileSize: fi.Size(),
	}

	reply := &serverplugin.FileTransferReply{}
	err = c.Call(ctx, "TransferFile", args, reply)
	if err != nil {
		return err
	}

	conn, err := net.DialTimeout("tcp", reply.Addr, c.option.ConnectTimeout)
	if err != nil {
		return err
	}

	defer conn.Close()

	_, err = conn.Write(reply.Token)
	if err != nil {
		return err
	}

	var tb *ratelimit.Bucket

	if rateInBytesPerSecond > 0 {
		tb = ratelimit.NewBucketWithRate(float64(rateInBytesPerSecond), rateInBytesPerSecond)
	}

	sendBuffer := make([]byte, FileTransferBufferSize)
loop:
	for {
		select {
		case <-ctx.Done():
		default:
			if tb != nil {
				tb.Wait(FileTransferBufferSize)
			}
			n, err := file.Read(sendBuffer)
			if err != nil {
				if err == io.EOF {
					return nil
				} else {
					return err
				}
			}
			if n == 0 {
				break loop
			}
			_, err = conn.Write(sendBuffer)
			if err != nil {
				if err == io.EOF {
					return nil
				} else {
					return err
				}
			}
		}
	}

	return nil
}

func (c *xClient) DownloadFile(ctx context.Context, requestFileName string, saveTo io.Writer) error {
	args := serverplugin.DownloadFileArgs{
		FileName: requestFileName,
	}

	reply := &serverplugin.FileTransferReply{}
	err := c.Call(ctx, "DownloadFile", args, reply)
	if err != nil {
		return err
	}

	conn, err := net.DialTimeout("tcp", reply.Addr, c.option.ConnectTimeout)
	if err != nil {
		return err
	}

	defer conn.Close()

	_, err = conn.Write(reply.Token)
	if err != nil {
		return err
	}

	buf := make([]byte, FileTransferBufferSize)
	r := bufio.NewReader(conn)
loop:
	for {
		select {
		case <-ctx.Done():
		default:
			n, er := r.Read(buf)
			if n > 0 {
				_, ew := saveTo.Write(buf[0:n])
				if ew != nil {
					err = ew
					break loop
				}
			}
			if er != nil {
				if er != io.EOF {
					err = er
				}
				break loop
			}
		}

	}

	return err
}

// Close closes this client and its underlying connnections to services.
func (c *xClient) Close() error {
	c.isShutdown = true

	var errs []error
	c.mu.Lock()
	for k, v := range c.cachedClient {
		e := v.Close()
		if e != nil {
			errs = append(errs, e)
		}

		delete(c.cachedClient, k)

	}
	c.mu.Unlock()

	go func() {
		defer func() {
			if r := recover(); r != nil {

			}
		}()

		c.discovery.RemoveWatcher(c.ch)
		close(c.ch)
	}()

	if len(errs) > 0 {
		return ex.NewMultiError(errs)
	}
	return nil
}


func (c *xClient) IsGo() bool {
	// 检查是否为go服务端	
	return c.isGo
}

func (c *xClient) GetNotGoServers() map[string]string {
	return c.notGoServers
}




