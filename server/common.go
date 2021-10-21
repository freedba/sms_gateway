package server

import "github.com/bwmarrin/snowflake"

var SeqMsgId uint32
var snowNode *snowflake.Node
var SignalExit = make(chan struct{})
var SeqId uint32
var msgIdChan chan uint64
var FakeGateway int

const runMode = "server"

type AccountsInfo struct {
	NickName        string              `json:"nick_name"`
	CmppJoinDestId  string              `json:"cmpp_join_dest_id"`
	CmppDestId      string              `json:"cmpp_dest_id"`
	AccountHost     string              `json:"account_host"`
	CmppPassword    string              `json:"cmpp_password"`
	Id              int64               `json:"id"`
	BusinessInfo    []*CmppBusinessInfo `json:"business_info"`
	IsneedReceipt   int64               `json:"isneed_receipt"`
	NeedReceiptType int64               `json:"need_receipt_type"`
	IsHaveSelected  int64               `json:"is_have_selected"`
	BusinessId      int64               `json:"business_id"`
	FlowVelocity    int                 `json:"flow_velocity"`
}

type CmppBusinessInfo struct {
	BusinessId        int64  `json:"business_id"`
	YidongChannelId   int64  `json:"yidong_channel_id"`
	LiantongChannelId int64  `json:"liantong_channel_id"`
	DianxinChannelId  int64  `json:"dianxin_channel_id"`
	Status            int64  `json:"status"`
	Deduct            string `json:"deduct"`
}

//提交的结构体最终入缓存队列的结构体
type ClientSubmitMessageToBase struct {
	Mobile       string   `json:"mobile"`
	Messagetotal string   `json:"messagetotal"`
	DevelopNo    string   `json:"develop_no"`
	ServiceId    string   `json:"Service_Id"`
	SourceAddr   string   `json:"Source_Addr"`
	SubmitTime   string   `json:"Submit_time"`
	Message      string   `json:"message"`
	SendMsgid    []string `json:"send_msgid"`
	Uid          int64    `json:"uid"`
	IsCheck      bool     `json:"is_check"`
	TaskNo       string   `json:"task_no"`
}

//cmpp入队列格式修正为和http格式一致
type HttpSubmitMessageInfo struct {
	Uid               int64  `json:"uid"`
	Source            string `json:"source"`
	YidongChannelId   int64  `json:"yidong_channel_id"`
	LiantongChannelId int64  `json:"liantong_channel_id"`
	DianxinChannelId  int64  `json:"dianxin_channel_id"`
	IsneedReceipt     int64  `json:"isneed_receipt"`
	NeedReceiptType   int64  `json:"need_receipt_type"`
	IsHaveSelected    int64  `json:"is_have_selected"`
	Deduct            string `json:"deduct"`
	DevelopNo         string `json:"develop_no"`
	RealNum           int64  `json:"real_num"`
	SendNum           int64  `json:"send_num"`
	TaskContent       string `json:"task_content"`
	SendStatus        int64  `json:"send_status"`
	From              int    `json:"from"`
	MobileContent     string `json:"mobile_content"`
	SendLength        int64  `json:"send_length"`
	FreeTrial         int64  `json:"free_trial"`
	AppointmentTime   int64  `json:"appointment_time"`
	SendMsgId         string `json:"send_msg_id"`
	Extra             string `json:"extra"`
	TaskNo            string `json:"task_no"`
	SubmitTime        int64  `json:"submit_time"`
	BusinessId        int    `json:"business_id"`
}
