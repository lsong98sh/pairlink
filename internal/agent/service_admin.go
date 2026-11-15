package agent

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

const (
	// serviceOpTimeout 是管理页等待 relay 确认服务注册/注销的超时。
	serviceOpTimeout = 3 * time.Second
	// maxServiceFieldLen 是表单值长度上限（与安全边界一致）。
	maxServiceFieldLen = 256
)

// errControlNotReady 表示控制连接尚未完成握手/首次注册。
var errControlNotReady = errors.New("agent: 控制连接尚未就绪，请稍后重试")

// validateServiceForm 校验服务ID/协议/本地目标，返回规范化后的映射。
//
// 规则（docs/contract.md §3.10）：ID 只允许 a-z0-9-_ 且 1..64，附加服务不能叫
// default（default 是主服务的保留 ID）；协议只允许 tcp/udp；地址必须能解析为
// host:port 且端口 1..65535，总长度不超过 256。
func validateServiceForm(id, proto, local string) (config.ServerService, error) {
	id = strings.TrimSpace(id)
	proto = strings.ToLower(strings.TrimSpace(proto))
	local = strings.TrimSpace(local)

	if len(id) > maxServiceFieldLen || len(local) > maxServiceFieldLen {
		return config.ServerService{}, fmt.Errorf("字段长度超过 %d 字节", maxServiceFieldLen)
	}
	if err := config.ValidateServiceID(id); err != nil {
		return config.ServerService{}, err
	}
	if strings.EqualFold(id, config.DefaultServiceID) {
		return config.ServerService{}, fmt.Errorf("服务ID default 是主服务保留ID，请在主服务表单修改")
	}
	if proto != "tcp" && proto != "udp" {
		return config.ServerService{}, fmt.Errorf("协议只允许 tcp / udp")
	}
	addr, err := config.NormalizeListenAddr(local)
	if err != nil {
		return config.ServerService{}, fmt.Errorf("本地目标地址非法: %w", err)
	}
	if len(addr) > maxServiceFieldLen {
		return config.ServerService{}, fmt.Errorf("本地目标地址超过 %d 字节", maxServiceFieldLen)
	}
	return config.ServerService{ID: id, Proto: proto, Addr: addr}, nil
}

// validateMainServiceForm 校验主服务（default）的协议与本地目标。
func validateMainServiceForm(proto, local string) (config.ServerService, error) {
	proto = strings.ToLower(strings.TrimSpace(proto))
	local = strings.TrimSpace(local)
	if len(local) > maxServiceFieldLen {
		return config.ServerService{}, fmt.Errorf("本地目标地址超过 %d 字节", maxServiceFieldLen)
	}
	if proto != "tcp" && proto != "udp" {
		return config.ServerService{}, fmt.Errorf("协议只允许 tcp / udp")
	}
	addr, err := config.NormalizeListenAddr(local)
	if err != nil {
		return config.ServerService{}, fmt.Errorf("本地目标地址非法: %w", err)
	}
	if len(addr) > maxServiceFieldLen {
		return config.ServerService{}, fmt.Errorf("本地目标地址超过 %d 字节", maxServiceFieldLen)
	}
	return config.ServerService{ID: config.DefaultServiceID, Proto: proto, Addr: addr}, nil
}

// readyControl 返回已就绪的控制连接；未连接/未就绪/正在重连时返回错误。
func (a *agent) readyControl() (*controlConn, error) {
	if a.ctx.Err() != nil {
		return nil, errors.New("agent: 正在关停")
	}
	if !a.ctrlReady.Load() {
		return nil, errControlNotReady
	}
	ctrl := a.controlConn()
	if ctrl == nil {
		return nil, errControlNotReady
	}
	return ctrl, nil
}

// registerServiceOn 在给定控制连接上注册服务并等待 MsgServiceRegistered。
func (a *agent) registerServiceOn(ctrl *controlConn, svc config.ServerService) (string, error) {
	ch := ctrl.beginServiceWait(svc.ID)
	defer ctrl.endServiceWait(ch)
	req := &protocol.RegisterService{ServiceID: svc.ID, Proto: svc.Proto, LocalHint: svc.Addr}
	if err := ctrl.write(protocol.MsgRegisterService, req); err != nil {
		return "", fmt.Errorf("发送服务注册失败: %w", err)
	}
	ack, err := a.waitServiceAck(ch, svc.ID)
	if err != nil {
		return "", err
	}
	if ack.typ != protocol.MsgServiceRegistered {
		return "", fmt.Errorf("relay 未确认服务 %s 注册", svc.ID)
	}
	return ack.udpMode, nil
}

// unregisterServiceOn 在给定控制连接上注销服务并等待 MsgServiceUnregistered。
func (a *agent) unregisterServiceOn(ctrl *controlConn, serviceID string) error {
	ch := ctrl.beginServiceWait(serviceID)
	defer ctrl.endServiceWait(ch)
	if err := ctrl.write(protocol.MsgUnregisterService, &protocol.UnregisterService{ServiceID: serviceID}); err != nil {
		return fmt.Errorf("发送服务注销失败: %w", err)
	}
	ack, err := a.waitServiceAck(ch, serviceID)
	if err != nil {
		return err
	}
	if ack.typ != protocol.MsgServiceUnregistered {
		return fmt.Errorf("relay 未确认服务 %s 注销", serviceID)
	}
	return nil
}

// waitServiceAck 等待服务变更确认，超时/ctx 取消/连接关闭都返回错误。
func (a *agent) waitServiceAck(ch chan serviceAck, serviceID string) (serviceAck, error) {
	timer := time.NewTimer(serviceOpTimeout)
	defer timer.Stop()
	select {
	case ack := <-ch:
		if ack.err != nil {
			return serviceAck{}, ack.err
		}
		return ack, nil
	case <-timer.C:
		return serviceAck{}, fmt.Errorf("等待 relay 确认服务 %s 超时（%s）", serviceID, serviceOpTimeout)
	case <-a.ctx.Done():
		return serviceAck{}, a.ctx.Err()
	}
}

// saveService 新增或修改一个附加服务：运行时 → relay → ini。
//
// 任一步失败都回滚本地服务表，保证内存与 relay/ini 尽量一致。
func (a *agent) saveService(id, proto, local string) error {
	svc, err := validateServiceForm(id, proto, local)
	if err != nil {
		return err
	}

	a.regMu.Lock()
	defer a.regMu.Unlock()

	ctrl, err := a.readyControl()
	if err != nil {
		return err
	}
	old, existed := a.svcStore.get(svc.ID)

	// 1) 先更新本地服务表（后续重连注册的就是新表）。
	a.svcStore.set(svc)
	// 2) 向 relay 注册并等待确认。
	udpMode, err := a.registerServiceOn(ctrl, svc)
	if err != nil {
		if existed {
			a.svcStore.set(old)
		} else {
			a.svcStore.remove(svc.ID)
		}
		return err
	}
	if udpMode != "" {
		a.setUDPMode(udpMode)
	}
	a.addRegisteredService(registeredService{Service: svc, UDPMode: a.udpModeSnapshot()})

	// 3) 成功后才写回 ini。
	if err := config.PatchValue(a.cfg.Path, "server", "service."+svc.ID, svc.Proto+":"+svc.Addr); err != nil {
		a.rollbackService(ctrl, svc.ID, old, existed)
		return fmt.Errorf("服务 %s 已在 relay 生效，但写回 ini 失败，已尝试回滚: %w", svc.ID, err)
	}
	a.log.Info("管理页保存服务", "event", "admin_action", "action", "agent-service-save",
		"service_id", svc.ID, "proto", svc.Proto, "old", old.Addr, "new", svc.Addr)
	return nil
}

// deleteService 删除一个附加服务：relay 注销 → 本地服务表 → ini。
func (a *agent) deleteService(id string) error {
	id = strings.TrimSpace(id)
	if len(id) > maxServiceFieldLen {
		return fmt.Errorf("字段长度超过 %d 字节", maxServiceFieldLen)
	}
	if err := config.ValidateServiceID(id); err != nil {
		return err
	}
	if strings.EqualFold(id, config.DefaultServiceID) {
		return fmt.Errorf("主服务 default 不能删除，请改用主服务表单")
	}

	a.regMu.Lock()
	defer a.regMu.Unlock()

	ctrl, err := a.readyControl()
	if err != nil {
		return err
	}
	old, existed := a.svcStore.get(id)
	if !existed {
		return fmt.Errorf("服务 %s 不存在", id)
	}

	// 1) relay 注销成功后才改本地表。
	if err := a.unregisterServiceOn(ctrl, id); err != nil {
		return err
	}
	a.svcStore.remove(id)
	a.removeRegisteredService(id)

	// 2) 写回 ini；失败则重新注册并恢复本地表。
	if err := config.DeleteKey(a.cfg.Path, "server", "service."+id); err != nil {
		a.svcStore.set(old)
		a.addRegisteredService(registeredService{Service: old, UDPMode: a.udpModeSnapshot()})
		if _, rerr := a.registerServiceOn(ctrl, old); rerr != nil {
			a.log.Warn("删除服务写盘失败后重新注册也失败", "event", "admin_action",
				"action", "agent-service-delete", "service_id", id, "err", rerr)
		}
		return fmt.Errorf("服务 %s 已从 relay 注销，但写回 ini 失败，已尝试回滚: %w", id, err)
	}
	a.log.Info("管理页删除服务", "event", "admin_action", "action", "agent-service-delete",
		"service_id", id, "old", old.Proto+":"+old.Addr, "new", "")
	return nil
}

// saveMainService 修改主服务（default）的 protocol/local 并写回 [server] 两行。
func (a *agent) saveMainService(proto, local string) error {
	svc, err := validateMainServiceForm(proto, local)
	if err != nil {
		return err
	}

	a.regMu.Lock()
	defer a.regMu.Unlock()

	ctrl, err := a.readyControl()
	if err != nil {
		return err
	}
	old, existed := a.svcStore.get(config.DefaultServiceID)

	a.svcStore.set(svc)
	udpMode, err := a.registerServiceOn(ctrl, svc)
	if err != nil {
		if existed {
			a.svcStore.set(old)
		} else {
			a.svcStore.remove(config.DefaultServiceID)
		}
		return err
	}
	if udpMode != "" {
		a.setUDPMode(udpMode)
	}
	a.addRegisteredService(registeredService{Service: svc, UDPMode: a.udpModeSnapshot()})

	oldProto, oldLocal := "", ""
	if existed {
		oldProto, oldLocal = old.Proto, old.Addr
	}
	revert := func() {
		a.rollbackService(ctrl, config.DefaultServiceID, old, existed)
		// 尽力把已经写下的键恢复原值，避免 protocol/local 一新一旧。
		if existed {
			_ = config.PatchValue(a.cfg.Path, "server", "protocol", oldProto)
			_ = config.PatchValue(a.cfg.Path, "server", "local", oldLocal)
			return
		}
		_ = config.DeleteKey(a.cfg.Path, "server", "protocol")
		_ = config.DeleteKey(a.cfg.Path, "server", "local")
	}

	// 先写 local 再写 protocol：两次写回都会用 ini 库重写该文件（注释/排版可能归一，
	// 原子替换且权限不变），失败时统一回滚。
	if err := config.PatchValue(a.cfg.Path, "server", "local", svc.Addr); err != nil {
		revert()
		return fmt.Errorf("主服务已在 relay 生效，但写回 [server] local 失败，已尝试回滚: %w", err)
	}
	if err := config.PatchValue(a.cfg.Path, "server", "protocol", svc.Proto); err != nil {
		revert()
		return fmt.Errorf("主服务已在 relay 生效，但写回 [server] protocol 失败，已尝试回滚: %w", err)
	}
	a.log.Info("管理页修改主服务", "event", "admin_action", "action", "agent-main-save",
		"service_id", config.DefaultServiceID,
		"proto", fmt.Sprintf("%s→%s", oldProto, svc.Proto),
		"old", oldLocal, "new", svc.Addr)
	return nil
}

// rollbackService 尽力把一次失败的变更恢复为旧状态（注册/注销 + 本地表）。
//
// 回滚本身失败只记 WARN，不掩盖原始错误。
func (a *agent) rollbackService(ctrl *controlConn, id string, old config.ServerService, existed bool) {
	if existed {
		a.svcStore.set(old)
		a.addRegisteredService(registeredService{Service: old, UDPMode: a.udpModeSnapshot()})
		if _, err := a.registerServiceOn(ctrl, old); err != nil {
			a.log.Warn("回滚服务注册失败", "event", "admin_action_rollback",
				"action", "restore", "service_id", id, "err", err)
		}
		return
	}
	a.svcStore.remove(id)
	a.removeRegisteredService(id)
	if err := a.unregisterServiceOn(ctrl, id); err != nil {
		a.log.Warn("回滚服务注销失败", "event", "admin_action_rollback",
			"action", "unregister", "service_id", id, "err", err)
	}
}
