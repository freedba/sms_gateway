package server

import (
	"bytes"
	"net"
	"sms_lib/models"
	"sms_lib/utils"
	"strconv"
	"sync"
)

type Sessions struct {
	ln         net.Listener
	Users      map[string][]string
	conns      map[string]int
	RemoteAddr string
	mLock      *sync.Mutex
}

func (sess *Sessions) GetUserConns(name string) int {
	var conns int64
	keyMaps := models.EtcdCli.GetPrefix("/SMSGateway")
	for k, v := range keyMaps {
		logger.Debug().Msgf("key: %s, val: %s, name: %s", k, v, name)
		if bytes.Contains([]byte(k), []byte(name)) {
			logger.Debug().Msgf("name: %s", name)
			if count, err := strconv.ParseInt(v, 10, 32); err == nil {
				conns += count
			}
		}
	}
	logger.Debug().Msgf("账号(%s) 当前已建立的总连接数:%d", name, conns)
	return int(conns)
}

func (sess *Sessions) UpdEtcdKey(name string) {
	key := "/SMSGateway/" + strconv.FormatInt(utils.NodeId, 10) + "/" + name
	val := strconv.Itoa(len(sess.Users[name]))
	if val == "0" {
		models.EtcdCli.Delete(key)
	} else {
		models.EtcdCli.Set(key, val, true)
	}
}

func (sess *Sessions) Add(name string, strPoint string) {
	sess.mLock.Lock()
	defer sess.mLock.Unlock()
	sess.Users[name] = append(sess.Users[name], strPoint)
	sess.UpdEtcdKey(name)
}

func (sess *Sessions) Done(name string, strPoint string) {
	sess.mLock.Lock()
	defer sess.mLock.Unlock()
	s := sess.Users[name]
	sess.Users[name] = utils.SliceRemove(s, strPoint)
	sess.UpdEtcdKey(name)
	if len(sess.Users[name]) == 0 {
		delete(sess.Users, name)
	}
}
