# sms_gateway
win vscode 编译linux平台运行程序
set CGO_ENABLED=0
set GOOS=linux
set GOARCH=amd64

powershell命令
$env:GOOS="linux"

编译去路径
go build -gcflags=-trimpath=$GOPATH -asmflags=-trimpath=$GOPATH -ldflags "-w -s" 
