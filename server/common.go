/*
 * @Author: jason hexie007@gmail.com
 * @Date: 2023-04-30 22:03:42
 * @LastEditors: jason hexie007@gmail.com
 * @LastEditTime: 2023-05-07 18:42:57
 * @Description:
 *
 * Copyright (c) 2023 by jason hexie007@gmail.com, All Rights Reserved.
 */
package server

import "sms_lib/utils"

// import "github.com/bwmarrin/snowflake"

var SeqMsgID uint32

// var snowNode *snowflake.Node
var SignalExit = make(chan struct{})
var SeqID uint32

// var MsgIdChan chan uint64
var FakeGateway int
var EtcdCli *utils.EtcdClient

const runMode = "server"

type AccountsInfo struct {
	NickName         string             `json:"nick_name"`
	CmppJoinDestID   string             `json:"cmpp_join_dest_id"`
	CmppDestID       string             `json:"cmpp_dest_id"`
	AccountHost      string             `json:"account_host"`
	CmppPassword     string             `json:"cmpp_password"`
	ID               int64              `json:"id"`
	BusinessInfo     []CmppBusinessInfo `json:"business_info"`
	IsNeedReceipt    int64              `json:"isneed_receipt"`
	NeedReceiptType  int64              `json:"need_receipt_type"`
	IsHaveSelected   int64              `json:"is_have_selected"`
	BusinessID       int64              `json:"business_id"`
	FlowVelocity     int                `json:"flow_velocity"`
	ConnFlowVelocity int                `json:"conn_flow_velocity"`
	FreeTrial        int64              `json:"free_trial"`
	MarketFreeTrial  int64              `json:"marketing_free_trial"`
}

type CmppBusinessInfo struct {
	BusinessID        int64  `json:"business_id"` // 营销/行业服务
	YidongChannelID   int64  `json:"yidong_channel_id"`
	LiantongChannelID int64  `json:"liantong_channel_id"`
	DianxinChannelID  int64  `json:"dianxin_channel_id"`
	Status            int64  `json:"status"` // 1服务可用。2服务不可用
	Deduct            string `json:"deduct"`
}

// 提交的结构体最终入缓存队列的结构体
type ClientSubmitMessageToBase struct {
	Mobile       string   `json:"mobile"`
	Messagetotal string   `json:"messagetotal"`
	DevelopNo    string   `json:"develop_no"`
	ServiceID    string   `json:"Service_Id"`
	SourceAddr   string   `json:"Source_Addr"`
	SubmitTime   string   `json:"Submit_time"`
	Message      string   `json:"message"`
	SendMsgiD    []string `json:"send_msgid"`
	UID          int64    `json:"uid"`
	IsCheck      bool     `json:"is_check"`
	TaskNo       string   `json:"task_no"`
}

// cmpp入队列格式修正为和http格式一致
type HTTPSubmitMessageInfo struct {
	UID               int64  `json:"uid"`
	Source            string `json:"source"`
	YidongChannelID   int64  `json:"yidong_channel_id"`
	LiantongChannelID int64  `json:"liantong_channel_id"`
	DianxinChannelID  int64  `json:"dianxin_channel_id"`
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
	SendMsgID         string `json:"send_msg_id"`
	Extra             string `json:"extra"`
	TaskNo            string `json:"task_no"`
	SubmitTime        int64  `json:"submit_time"`
	BusinessID        int    `json:"business_id"`
}
