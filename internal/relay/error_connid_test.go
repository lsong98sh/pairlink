package relay

import (
	"testing"

	"github.com/lsong98sh/pairlink/internal/protocol"
)

// TestConnectRequestErrorCarriesClientConnID 证明 relay 在应答 MsgConnectRequest
// 的错误里填了 client_conn_id：client 据此精确关闭对应本地连接。
//
// 旧 client 会忽略该未知字段，行为不变（docs/protocol.md §4.12 的向后兼容约定）。
func TestConnectRequestErrorCarriesClientConnID(t *testing.T) {
	s := startTestServer(t, testConfig(t, nil))

	agent := dialControl(t, s.tunnelAddr)
	agent.hello(protocol.RoleAgent, testToken)
	agent.register("svc-a", "tcp")

	client := dialControl(t, s.tunnelAddr)
	client.hello(protocol.RoleClient, testToken)

	// 1) 服务不存在：错误必须带上发起该请求的 client_conn_id。
	client.send(protocol.MsgConnectRequest, &protocol.ConnectRequest{
		ServiceID:    "ghost",
		ClientConnID: "conn-not-found",
	})
	if e := client.expectError(protocol.ErrCodeServiceNotFound); e.ClientConnID != "conn-not-found" {
		t.Fatalf("service_not_found 的 client_conn_id = %q, want conn-not-found", e.ClientConnID)
	}

	// 2) 非法 service_id：同样带关联 ID（客户端能定位到具体连接）。
	client.send(protocol.MsgConnectRequest, &protocol.ConnectRequest{
		ServiceID:    "BAD ID",
		ClientConnID: "conn-bad",
	})
	if e := client.expectError(protocol.ErrCodeBadRequest); e.ClientConnID != "conn-bad" {
		t.Fatalf("bad_request 的 client_conn_id = %q, want conn-bad", e.ClientConnID)
	}

	// 3) 与 ConnectRequest 无关的错误（agent 注册非法服务）不得带关联 ID，
	//    避免 client 误关某个正在等待的本地连接。
	agent.send(protocol.MsgRegisterService, &protocol.RegisterService{
		ServiceID: "BAD ID",
		Proto:     "tcp",
	})
	if e := agent.expectError(protocol.ErrCodeBadRequest); e.ClientConnID != "" {
		t.Fatalf("与 ConnectRequest 无关的错误不应带 client_conn_id，实际 %q", e.ClientConnID)
	}
}
