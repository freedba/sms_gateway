package server

import (
	"context"
	"go.etcd.io/etcd/client/v3"
	"sms_lib/config"
	"sms_lib/utils"
	"strings"
	"time"
)

type EtcdClient struct {
	endpoints []string
	EtcdCli   *clientv3.Client
	kv        clientv3.KV
	leaseId   clientv3.LeaseID
	StopLease chan struct{}

	timeout time.Duration
}

func NewEtcd() {
	var hosts []string
	tmp := utils.GetEnv("ETCD_HOSTS")
	if tmp == "" {
		hosts = config.GetEtcd()
	} else {
		hosts = strings.Split(tmp, ",")
	}
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   hosts,
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		logger.Error().Msgf("etcd初始化错误:%v", err)
		return
	}

	EtcdCli = &EtcdClient{
		endpoints: hosts,
		EtcdCli:   cli,
		kv:        clientv3.NewKV(cli),
		timeout:   5 * time.Second,
	}
	logger.Info().Msgf("etcd初始化完成:%v", EtcdCli)
}

func (self *EtcdClient) LeaseGrant(ttl int64) {
	cli := self.EtcdCli
	ctx, cancel := context.WithTimeout(context.Background(), self.timeout)
	defer cancel()
	resp, err := cli.Grant(ctx, ttl)
	if err != nil {
		logger.Panic().Msgf("Lease error:%v", err)
	}
	logger.Info().Msgf("etcd lease:%x", resp.ID)
	self.leaseId = resp.ID
}

func (self *EtcdClient) LeaseRenew() {
	cli := self.EtcdCli
	ctx, cancel := context.WithTimeout(context.Background(), self.timeout)
	ch, kaErr := cli.KeepAlive(ctx, self.leaseId)
	cancel()
	if kaErr != nil {
		logger.Panic().Msgf("Lease renew error:%v", kaErr)
	}
	for {
		select {
		case <-self.StopLease:
			logger.Error().Msgf("stop lease renew")
			break
		case ka := <-ch:
			if ka == nil {
				logger.Panic().Msgf("keep alive failed: %v", ka)
			} else {
				logger.Debug().Msgf("ka:%v", ka)
			}
		}
	}
}

func (self *EtcdClient) LeaseTTL() {
	cli := self.EtcdCli
	ctx, cancel := context.WithTimeout(context.Background(), self.timeout)
	defer cancel()
	resp, err := cli.TimeToLive(ctx, self.leaseId)
	if err != nil {
		logger.Error().Msgf("cli.TimeToLive error:%v", err)
		return
	}
	logger.Info().Msgf("LeaseTTL:%v, ID:%d,ttl:%d,GrantedTTL:%d,keys:%v",
		resp, resp.ID, resp.TTL, resp.GrantedTTL, resp.Keys)
}

func (self *EtcdClient) Set(key string, value string, isLease bool) {
	var err error
	kv := self.kv
	logger.Debug().Msgf("timeout:%v", self.timeout)
	ctx, cancel := context.WithTimeout(context.Background(), self.timeout)
	defer cancel()
	if isLease {
		_, err = kv.Put(ctx, key, value, clientv3.WithLease(self.leaseId))
	} else {
		_, err = kv.Put(ctx, key, value)
	}
	if err != nil {
		logger.Error().Msgf("put to etcd failed, err:%v", err)
	}
}

func (self *EtcdClient) GetPrefix(key string) map[string]string {
	var keyMap = make(map[string]string)
	kv := self.kv
	ctx, cancel := context.WithTimeout(context.Background(), self.timeout)
	defer cancel()
	resp, err := kv.Get(ctx, key, clientv3.WithPrefix())
	if err != nil {
		logger.Error().Msgf("cli.get error: %v", err)
	}
	for _, ev := range resp.Kvs {
		keyMap[string(ev.Key)] = string(ev.Value)
	}
	return keyMap
}

func (self *EtcdClient) Delete(prefixKey string) {
	logger.Debug().Msgf("delete key:%s", prefixKey)
	kv := self.kv
	var err error
	ctx, cancel := context.WithTimeout(context.Background(), self.timeout)
	defer cancel()
	_, err = kv.Delete(ctx, prefixKey, clientv3.WithPrefix())
	if err != nil {
		logger.Error().Msgf("delete key(%s) error:%v", prefixKey, err)
	}
}
