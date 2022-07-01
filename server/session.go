package server

import (
	"bytes"
	"net"
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

func (sess *Sessions) GetUserConn(name string) int {
	var conn int64
	keyMaps := EtcdCli.GetPrefix("/SMSGateway")
	for k, v := range keyMaps {
		logger.Debug().Msgf("key: %s, val: %s, name: %s", k, v, name)
		if bytes.Contains([]byte(k), []byte(name)) {
			logger.Debug().Msgf("name: %s", name)
			if count, err := strconv.ParseInt(v, 10, 32); err == nil {
				conn += count
			}
		}
	}
	logger.Debug().Msgf("账号(%s) 当前已建立的总连接数:%d", name, conn)
	return int(conn)
}

func (sess *Sessions) UpdEtcdKey(name string) {
	key := "/SMSGateway/" + strconv.FormatInt(utils.NodeId, 10) + "/" + name
	val := strconv.Itoa(len(sess.Users[name]))
	if val == "0" {
		EtcdCli.Delete(key)
	} else {
		EtcdCli.Set(key, val, true)
	}
}

func (sess *Sessions) Close(user string) {
	if strPoints, ok := sess.Users[user]; ok {
		for i := 0; i < len(strPoints); i++ {
			runId := user + ":" + sess.Users[user][i]
			logger.Debug().Msgf("关闭账号:%s", runId)
			close(utils.ExitSig.LoopRead[runId])
		}
	}
}

func (sess *Sessions) Add(user string, strPoint string) {
	sess.mLock.Lock()
	defer sess.mLock.Unlock()
	sess.Users[user] = append(sess.Users[user], strPoint)
	sess.UpdEtcdKey(user)
}

func (sess *Sessions) Done(user string, strPoint string) {
	sess.mLock.Lock()
	defer sess.mLock.Unlock()
	if strPoints, ok := sess.Users[user]; ok {
		sess.Users[user] = utils.SliceRemove(strPoints, strPoint)
		sess.UpdEtcdKey(user)
		if len(sess.Users[user]) == 0 {
			delete(sess.Users, user)
		}
	}
}
