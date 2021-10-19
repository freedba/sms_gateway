package server

import (
	"fmt"
	"github.com/bwmarrin/snowflake"
	"sms_lib/config"
	"sms_lib/utils"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

func InitChan(runId string) {
	utils.ExitSig.LoopActiveTest[runId] = make(chan bool, 1)
	utils.ExitSig.DeliverySender[runId] = make(chan bool, 1)
	//utils.ExitSig.LoopRead[id] = make(chan bool, 1)
	//utils.ExitSig.LoopSend[id] = make(chan bool, 1)
	//utils.ExitSig.SubmitRespMsgIdToQueue[id] = make(chan bool, 1)l
	//utils.ExitSig.DeliveryMsgIdToQueue[id] = make(chan bool, 1)
	//utils.ExitSig.SubmitRespMsgIdToDB[id] = make(chan bool, 1)
	//utils.ExitSig.DeliveryMsgIdToDB[id] = make(chan bool, 1)
	//utils.ExitSig.HandleCommand[id] = make(chan bool, 1)
	utils.HbSeqId.RespSeqId[runId] = make(chan uint32, 1)
	utils.HbSeqId.SeqId[runId] = make(chan uint32, 1)
	//utils.NotifySig.IsTransfer[id] = make(chan bool, 1)
	//utils.NotifySig.SendMessage[id] = make(chan bool, 1)
	qlen := config.GetQlen()
	if qlen < 16 {
		qlen = 16
	}
	logger.Debug().Msgf("队列缓冲值：%d", qlen)
	//HandleSeqId.SubmitResp[id] = make(chan protocol.SubmitResp, qlen)
	//HandleSeqId.Deliver[id] = make(chan protocol.Deliver, qlen)
	//HandleSeqId.Command[id] = make(chan []byte, qlen)
}

func NewSnowflakeNode() {
	var err error
	ns := time.Date(2021, 1, 0, 0, 0, 0, 0, time.UTC).UnixNano()
	ms := ns / 1e6
	snowflake.Epoch = ms
	logger.Debug().Msgf("snowflake.Epoch:%d", snowflake.Epoch)
	snowNode, err = snowflake.NewNode(utils.NodeId)
	if err != nil {
		logger.Panic().Msgf("New Snowflake Node err:%v", err)
	}
}

func generateMsgID() uint64 {
	msgId := snowNode.Generate().Int64()
	//logger.Debug().Msgf("generate msgid: %d",msgId)
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