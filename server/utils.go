package server

import (
	"sync"
)

type WaitGroupWrapper struct {
	sync.WaitGroup
}

func (w *WaitGroupWrapper) Wrap(f func()) {
	w.Add(1)
	go func() {
		f()
		w.Done()
	}()
}

type LongSms struct {
	Content map[uint8][]byte
	MsgID   map[uint8]string
	mLock   *sync.Mutex
}

func (ls *LongSms) set(k uint8, msgID string, content []byte) {
	ls.mLock.Lock()
	defer ls.mLock.Unlock()
	ls.MsgID[k] = msgID
	ls.Content[k] = content
}

func (ls *LongSms) len() uint8 {
	ls.mLock.Lock()
	defer ls.mLock.Unlock()
	return uint8(len(ls.MsgID))
}

func (ls *LongSms) get(k uint8) (msgID string, content []byte) {
	ls.mLock.Lock()
	defer ls.mLock.Unlock()
	msgID, _ = ls.MsgID[k]
	content, _ = ls.Content[k]
	return msgID, content
}

func (ls *LongSms) exist(k uint8) bool {
	ls.mLock.Lock()
	defer ls.mLock.Unlock()
	_, ok := ls.MsgID[k]
	return ok
}

type LongSmsMap struct {
	LongSms   map[uint8]*LongSms
	Timestamp int64
	mLock     *sync.Mutex
}

func (lsm *LongSmsMap) print() map[uint8]*LongSms {
	lsm.mLock.Lock()
	defer lsm.mLock.Unlock()
	return lsm.LongSms
}

func (lsm *LongSmsMap) get(k uint8) *LongSms {
	lsm.mLock.Lock()
	defer lsm.mLock.Unlock()
	val, ok := lsm.LongSms[k]
	if ok {
		return val
	}
	return nil
}

func (lsm *LongSmsMap) set(k uint8, v *LongSms) {
	lsm.mLock.Lock()
	defer lsm.mLock.Unlock()
	lsm.LongSms[k] = v
}

func (lsm *LongSmsMap) exist(k uint8) bool {
	lsm.mLock.Lock()
	defer lsm.mLock.Unlock()
	_, ok := lsm.LongSms[k]
	return ok
}

func (lsm *LongSmsMap) del(k uint8) {
	lsm.mLock.Lock()
	defer lsm.mLock.Unlock()
	delete(lsm.LongSms, k)
}

func (lsm *LongSmsMap) len() uint8 {
	lsm.mLock.Lock()
	defer lsm.mLock.Unlock()
	return uint8(len(lsm.LongSms))
}
