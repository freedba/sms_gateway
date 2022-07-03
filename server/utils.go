package server

import (
	"fmt"
	"github.com/bwmarrin/snowflake"
	"sms_lib/utils"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

func NewSnowflakeNode() {
	var err error
	ns := time.Date(2021, 1, 0, 0, 0, 0, 0, time.UTC).UnixNano()
	ms := ns / 1e6
	snowflake.Epoch = ms
	logger.Debug().Msgf("snowflake.Epoch:%d,utils.NodeId:%d", snowflake.Epoch, utils.NodeId)
	snowNode, err = snowflake.NewNode(utils.NodeId)
	if err != nil {
		logger.Panic().Msgf("New Snowflake Node err:%v", err)
	}
}

func GenerateMsgID() uint64 {
	msgId := snowNode.Generate().Int64()
	return uint64(msgId)
}

func makeMsgID() uint64 {
	//信息标识，生成算法如下：
	//采用 64 位（ 8 字节）的整数：
	//（ 1） 时间（格式为 MMDDHHMMSS，即
	//月日时分秒）： bit64~bit39，其中
	//bit64~bit61：月份的二进制表示；占4位
	//bit60~bit56：日的二进制表示；占5位
	//bit55~bit51：小时的二进制表示；占5位
	//bit50~bit45：分的二进制表示；占6位
	//bit44~bit39：秒的二进制表示；占6位
	//（ 2） 短信网关代码： bit38~bit17，把短信
	//网关的代码转换为整数填写到该字
	//段中。占22位-->6位
	//（ 3） 序列号： bit16~bit1，顺序增加，步
	//长为 1，循环使用。占16位-->32位
	//各部分如不能填满，左补零，右对齐。
	var msgID uint64
	var bin string
	var allBin string
	seqID := atomic.AddUint32(&SeqMsgId, 1)
	t := time.Now().Format("0102150405")
	for idx, str := range utils.SplitSubN(t, 2) {
		num, _ := strconv.ParseInt(str, 10, 16)
		if idx == 0 {
			bin = fmt.Sprintf("%04b", num)
		} else if idx >= 1 && idx <= 2 {
			bin = fmt.Sprintf("%05b", num)
		} else {
			bin = fmt.Sprintf("%06b", num)
		}
		allBin += bin
	}
	bin = fmt.Sprintf("%06b", 1) //固定填充
	allBin += bin
	bin = fmt.Sprintf("%032b", seqID)
	allBin += bin
	msgID, _ = strconv.ParseUint(allBin, 2, 64)
	return msgID
}

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
