/**
 * Copyright (c) 2014-2015, GoBelieve     
 * All rights reserved.
 *
 * This program is free software; you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation; either version 2 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program; if not, write to the Free Software
 * Foundation, Inc., 59 Temple Place, Suite 330, Boston, MA  02111-1307  USA
 */

package main
import "net"
import "fmt"
import "flag"
import "time"
import "runtime"
import "net/http"
import "strings"
import "strconv"
import "sync/atomic"
import "github.com/garyburd/redigo/redis"
import log "github.com/golang/glog"

//group storage server
var storage_channels []*StorageChannel

//route server
var route_channels []*Channel

//storage server
var channels []*Channel

var group_center *GroupCenter

var app_route *AppRoute
var group_manager *GroupManager
var redis_pool *redis.Pool
var storage_pools []*StorageConnPool
var config *Config
var server_summary *ServerSummary
var customer_service *CustomerService

func init() {
	app_route = NewAppRoute()
	server_summary = NewServerSummary()
	group_center = NewGroupCenter()
}

func handle_client(conn net.Conn) {
	log.Infoln("handle_client")
	client := NewClient(conn)
	client.Run()
}

func Listen(f func(net.Conn), port int) {
	TCPService(fmt.Sprintf("0.0.0.0:%d", port), f)

}
func ListenClient() {
	Listen(handle_client, config.port)
}

func NewRedisPool(server, password string, db int) *redis.Pool {
	return &redis.Pool{
		MaxIdle:     100,
		MaxActive:   500,
		IdleTimeout: 480 * time.Second,
		Dial: func() (redis.Conn, error) {
			c, err := redis.Dial("tcp", server)
			if err != nil {
				return nil, err
			}
			if len(password) > 0 {
				if _, err := c.Do("AUTH", password); err != nil {
					c.Close()
					return nil, err
				}
			}
			if db > 0 && db < 16 {
				if _, err := c.Do("SELECT", db); err != nil {
					c.Close()
					return nil, err
				}
			}
			return c, err
		},
	}
}

func GetStorageConnPool(uid int64) *StorageConnPool {
	index := uid%int64(len(storage_pools))
	return storage_pools[index]
}

func GetGroupStorageConnPool(gid int64) *StorageConnPool {
	index := gid%int64(len(storage_pools))
	return storage_pools[index]
}

func GetGroupStorageChannel(gid int64) *StorageChannel {
	index := gid%int64(len(storage_channels))
	return storage_channels[index]
}

func GetChannel(uid int64) *Channel{
	index := uid%int64(len(route_channels))
	return route_channels[index]
}

func GetRoomChannel(room_id int64) *Channel {
	index := room_id%int64(len(route_channels))
	return route_channels[index]
}

func GetUserStorageChannel(uid int64) *Channel {
	index := uid%int64(len(channels))
	return channels[index]
}

func SaveGroupMessage(appid int64, gid int64, device_id int64, m *Message) (int64, error) {
	log.Infof("save group message:%d %d\n", appid, gid)
	storage_pool := GetGroupStorageConnPool(gid)
	storage, err := storage_pool.Get()
	if err != nil {
		log.Error("connect storage err:", err)
		return 0, err
	}
	defer storage_pool.Release(storage)

	sae := &SAEMessage{}
	sae.msg = m
	sae.appid = appid
	sae.receiver = gid
	sae.device_id = device_id

	msgid, err := storage.SaveAndEnqueueGroupMessage(sae)
	if err != nil {
		log.Error("saveandequeue message err:", err)
		return 0, err
	}
	return msgid, nil
}

func SaveMessage(appid int64, uid int64, device_id int64, m *Message) (int64, error) {
	storage_pool := GetStorageConnPool(uid)
	storage, err := storage_pool.Get()
	if err != nil {
		log.Error("connect storage err:", err)
		return 0, err
	}
	defer storage_pool.Release(storage)

	sae := &SAEMessage{}
	sae.msg = m
	sae.appid = appid
	sae.receiver = uid
	sae.device_id = device_id

	msgid, err := storage.SaveAndEnqueueMessage(sae)
	if err != nil {
		log.Error("saveandequeue message err:", err)
		return 0, err
	}
	return msgid, nil
}

func Send0Message(appid int64, uid int64, msg *Message) bool {
	amsg := &AppMessage{appid:appid, receiver:uid, msgid:0, msg:msg}
	SendAppMessage(amsg, uid)
	return true
}

func SendAppMessage(amsg *AppMessage, uid int64) bool {
	channel := GetChannel(uid)
	channel.Publish(amsg)

	route := app_route.FindRoute(amsg.appid)
	if route == nil {
		log.Warningf("can't dispatch app message, appid:%d uid:%d cmd:%s", amsg.appid, amsg.receiver, Command(amsg.msg.cmd))
		return false
	}
	clients := route.FindClientSet(uid)
	if len(clients) == 0 {
		log.Warningf("can't dispatch app message, appid:%d uid:%d cmd:%s", amsg.appid, amsg.receiver, Command(amsg.msg.cmd))
		return false
	}

	for c, _ := range(clients) {
		if amsg.msgid > 0 {
			c.EnqueueEMessage(&EMessage{msgid:amsg.msgid, msg:amsg.msg})
		} else {
			c.EnqueueMessage(amsg.msg)
		}
	}

	return true
}

func DispatchAppMessage(amsg *AppMessage) {
	log.Info("dispatch app message:", Command(amsg.msg.cmd))

	route := app_route.FindRoute(amsg.appid)
	if route == nil {
		log.Warningf("can't dispatch app message, appid:%d uid:%d cmd:%s", amsg.appid, amsg.receiver, Command(amsg.msg.cmd))
		return
	}
	clients := route.FindClientSet(amsg.receiver)
	if len(clients) == 0 {
		log.Warningf("can't dispatch app message, appid:%d uid:%d cmd:%s", amsg.appid, amsg.receiver, Command(amsg.msg.cmd))
		return
	}
	for c, _ := range(clients) {
		//自己在同一台设备上发出的消息，不再发送回去
		if amsg.msg.cmd == MSG_IM || amsg.msg.cmd == MSG_GROUP_IM {
			m := amsg.msg.body.(*IMMessage)
			if m.sender == amsg.receiver && amsg.device_id == c.device_ID {
				continue
			}
		}

		if amsg.msg.cmd == MSG_CUSTOMER {
			m := amsg.msg.body.(*CustomerMessage)

			if m.customer_appid == c.appid && m.customer_id == amsg.receiver && amsg.device_id == c.device_ID {
				continue
			}
		}

		if amsg.msg.cmd == MSG_CUSTOMER_SUPPORT {
			m := amsg.msg.body.(*CustomerMessage)
			if m.customer_appid != c.appid && m.seller_id == amsg.receiver && amsg.device_id == c.device_ID {
				continue
			}
		}

		if amsg.msgid > 0 {
			c.EnqueueEMessage(&EMessage{msgid:amsg.msgid, msg:amsg.msg})
		} else {
			c.EnqueueMessage(amsg.msg)
		}
	}
}

func DispatchRoomMessage(amsg *AppMessage) {
	log.Info("dispatch room message", Command(amsg.msg.cmd))
	room_id := amsg.receiver
	route := app_route.FindOrAddRoute(amsg.appid)
	clients := route.FindRoomClientSet(room_id)

	if len(clients) == 0 {
		log.Warningf("can't dispatch room message, appid:%d room id:%d cmd:%s", amsg.appid, amsg.receiver, Command(amsg.msg.cmd))
		return
	}
	for c, _ := range(clients) {
		c.EnqueueMessage(amsg.msg)
	}	
}

func DispatchGroupMessage(amsg *AppMessage) {
	log.Info("dispatch group message:", Command(amsg.msg.cmd))
	group := group_manager.FindGroup(amsg.receiver)
	if group == nil {
		log.Warningf("can't dispatch group message, appid:%d group id:%d", amsg.appid, amsg.receiver)
		return
	}

	route := app_route.FindRoute(amsg.appid)
	if route == nil {
		log.Warningf("can't dispatch app message, appid:%d uid:%d cmd:%s", amsg.appid, amsg.receiver, Command(amsg.msg.cmd))
		return
	}

	members := group.Members()
	for member := range members {
	    clients := route.FindClientSet(member)
		if len(clients) == 0 {
			continue
		}

		for c, _ := range(clients) {
			if amsg.msg.cmd == MSG_GROUP_IM {
				im := amsg.msg.body.(*IMMessage)
				
				//不再发送给发送者所在的设备
				if c.uid == im.sender && c.device_ID == amsg.device_id {
					continue
				}
			}
			c.EnqueueEMessage(&EMessage{msgid:amsg.msgid, msg:amsg.msg})
		}
	}
}

func DialStorageFun(addr string) func()(*StorageConn, error) {
	f := func() (*StorageConn, error){
		storage := NewStorageConn()
		err := storage.Dial(addr)
		if err != nil {
			log.Error("connect storage err:", err)
			return nil, err
		}
		return storage, nil
	}
	return f
}

type IMGroupObserver int
func (ob IMGroupObserver) OnGroupMemberAdd(group *Group, uid int64) {
	group_center.SubscribeGroupMember(group.appid, group.gid, uid)
}

func (ob IMGroupObserver) OnGroupMemberRemove(group *Group, uid int64) {
	group_center.UnsubscribeGroupMember(group.appid, group.gid, uid)
}


type loggingHandler struct {
	handler http.Handler
}

func (h loggingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Infof("http request:%s %s %s", r.RemoteAddr, r.Method, r.URL)
	h.handler.ServeHTTP(w, r)
}

func StartHttpServer(addr string) {
	http.HandleFunc("/summary", Summary)
	http.HandleFunc("/stack", Stack)

	//rpc function
	http.HandleFunc("/post_group_notification", PostGroupNotification)
	http.HandleFunc("/post_im_message", PostIMMessage)
	http.HandleFunc("/load_latest_message", LoadLatestMessage)
	http.HandleFunc("/load_history_message", LoadHistoryMessage)
	http.HandleFunc("/post_system_message", SendSystemMessage)
	http.HandleFunc("/post_room_message", SendRoomMessage)
	http.HandleFunc("/post_customer_message", SendCustomerMessage)
	http.HandleFunc("/post_realtime_message", SendRealtimeMessage)
	http.HandleFunc("/init_message_queue", InitMessageQueue)

	handler := loggingHandler{http.DefaultServeMux}
	HTTPService(addr, handler)
}

func HandleForbidden(data string) {
	arr := strings.Split(data, ",")
	if len(arr) != 3 {
		log.Info("message error:", data)
		return
	}
	appid, err := strconv.ParseInt(arr[0], 10, 64)
	if err != nil {
		log.Info("error:", err)
		return
	}
	uid, err := strconv.ParseInt(arr[1], 10, 64)
	if err != nil {
		log.Info("error:", err)
		return
	}
	fb, err := strconv.ParseInt(arr[2], 10, 64)
	if err != nil {
		log.Info("error:", err)
		return
	}

	route := app_route.FindRoute(appid)
	if route == nil {
		log.Warningf("can't find appid:%d route", appid)
		return
	}
	clients := route.FindClientSet(uid)
	if len(clients) == 0 {
		return
	}

	log.Infof("forbidden:%d %d %d client count:%d", 
		appid, uid, fb, len(clients))
	for c, _ := range(clients) {
		atomic.StoreInt32(&c.forbidden, int32(fb))
	}
}

func SubscribeRedis() bool {
	c, err := redis.Dial("tcp", config.redis_address)
	if err != nil {
		log.Info("dial redis error:", err)
		return false
	}

	password := config.redis_password
	if len(password) > 0 {
		if _, err := c.Do("AUTH", password); err != nil {
			c.Close()
			return false
		}
	}

	psc := redis.PubSubConn{c}
	psc.Subscribe("store_update", "speak_forbidden")

	customer_service.Clear()
	for {
		switch v := psc.Receive().(type) {
		case redis.Message:
			log.Infof("%s: message: %s\n", v.Channel, v.Data)
			if v.Channel == "store_update" {
				customer_service.HandleMessage(&v)
			} else if v.Channel == "speak_forbidden" {
				HandleForbidden(string(v.Data))
			}
		case redis.Subscription:
			log.Infof("%s: %s %d\n", v.Channel, v.Kind, v.Count)
		case error:
			log.Info("error:", v)
			return true
		}
	}
}

func ListenRedis() {
	nsleep := 1
	for {
		connected := SubscribeRedis()
		if !connected {
			nsleep *= 2
			if nsleep > 60 {
				nsleep = 60
			}
		} else {
			nsleep = 1
		}
		time.Sleep(time.Duration(nsleep) * time.Second)
	}
}



func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())
	flag.Parse()
	if len(flag.Args()) == 0 {
		fmt.Println("usage: im config")
		return
	}

	config = read_cfg(flag.Args()[0])
	log.Infof("port:%d redis address:%s\n",
		config.port,  config.redis_address)

	log.Infof("redis address:%s password:%s db:%d\n", 
		config.redis_address, config.redis_password, config.redis_db)

	log.Info("storage addresses:", config.storage_addrs)
	log.Info("route addressed:", config.route_addrs)
	log.Info("kefu appid:", config.kefu_appid)
	
	customer_service = NewCustomerService()

	redis_pool = NewRedisPool(config.redis_address, config.redis_password, 
		config.redis_db)

	storage_pools = make([]*StorageConnPool, 0)
	for _, addr := range(config.storage_addrs) {
		f := DialStorageFun(addr)
		pool := NewStorageConnPool(100, 500, 600 * time.Second, f) 
		storage_pools = append(storage_pools, pool)
	}

	storage_channels = make([]*StorageChannel, 0)

	for _, addr := range(config.storage_addrs) {
		sc := NewStorageChannel(addr, DispatchGroupMessage)
		sc.Start()
		storage_channels = append(storage_channels, sc)
	}

	channels = make([]*Channel, 0)
	for _, addr := range(config.storage_addrs) {
		channel := NewChannel(addr, DispatchAppMessage, nil)
		channel.Start()
		channels = append(channels, channel)
	}

	route_channels = make([]*Channel, 0)
	for _, addr := range(config.route_addrs) {
		channel := NewChannel(addr, DispatchAppMessage, DispatchRoomMessage)
		channel.Start()
		route_channels = append(route_channels, channel)
	}
	
	group_manager = NewGroupManager()
	group_manager.observer = IMGroupObserver(0)
	group_manager.Start()

	go ListenRedis()

	StartHttpServer(config.http_listen_address)

	go StartSocketIO(config.socket_io_address)
	ListenClient()
	Wait()
}
