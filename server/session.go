package server

import (
	"bytes"
	"fmt"
	"net"
	"sms_lib/utils"
	"strconv"
	"sync"
)

type Sessions struct {
	ln         net.Listener
	Users      map[string][]*SrvConn
	conns      map[string]int
	RemoteAddr string
	mLock      *sync.Mutex
}

func (sess *Sessions) GetUserConn(name string) int {
	var conn int64
	keyMaps := EtcdCli.GetPrefix("/SMSGateway")
	if keyMaps == nil {

	}
	for k, v := range keyMaps {
		logger.Debug().Msgf("key: %s, val: %s, name: %s", k, v, name)
		if bytes.Contains([]byte(k), []byte(name)) {
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
	if srvList, ok := sess.Users[user]; ok {
		for i := 0; i < len(srvList); i++ {
			s := sess.Users[user][i]
			runId := user + ":" + fmt.Sprintf("%p", s.conn)
			logger.Debug().Msgf("关闭账号:%s,close(s.ExitSrv)", runId)
			if !utils.ChIsClosed(s.ExitSrv) {
				utils.CloseChan(&s.ExitSrv, s.mutex)
			}
		}
	}
}

func (sess *Sessions) Add(user string, srv *SrvConn) {
	sess.mLock.Lock()
	defer sess.mLock.Unlock()
	sess.Users[user] = append(sess.Users[user], srv)
	sess.UpdEtcdKey(user)
}

func (sess *Sessions) Done(user string, srv *SrvConn) {
	sess.mLock.Lock()
	defer sess.mLock.Unlock()
	if srvList, ok := sess.Users[user]; ok {
		sess.Users[user] = sess.SliceRemove(srvList, srv)
		sess.UpdEtcdKey(user)
		if len(sess.Users[user]) == 0 {
			delete(sess.Users, user)
		}
	}
}

func (sess *Sessions) SliceRemove(s []*SrvConn, r *SrvConn) []*SrvConn {
	for i, v := range s {
		if v == r {
			return append(s[:i], s[i+1:]...)
		}
	}
	return s
}
