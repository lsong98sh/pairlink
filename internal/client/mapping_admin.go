package client

import (
	"errors"
	"fmt"
	"strings"

	"github.com/lsong98sh/pairlink/internal/config"
)

// maxMappingFieldLen 是映射表单值长度上限。
const maxMappingFieldLen = 256

// mappingSnapshot 返回本地映射表副本。
func (c *client) mappingSnapshot() []config.ClientService {
	c.mappingMu.Lock()
	defer c.mappingMu.Unlock()
	return append([]config.ClientService(nil), c.mappings...)
}

// mappingByID 返回指定映射。
func (c *client) mappingByID(id string) (config.ClientService, bool) {
	c.mappingMu.Lock()
	defer c.mappingMu.Unlock()
	for _, m := range c.mappings {
		if m.ID == id {
			return m, true
		}
	}
	return config.ClientService{}, false
}

// setMapping 新增或更新映射（保留首次出现顺序）。
func (c *client) setMapping(svc config.ClientService) {
	c.mappingMu.Lock()
	for i := range c.mappings {
		if c.mappings[i].ID == svc.ID {
			c.mappings[i] = svc
			c.mappingMu.Unlock()
			return
		}
	}
	c.mappings = append(c.mappings, svc)
	c.mappingMu.Unlock()
}

// removeMapping 删除映射（不存在时无操作）。
func (c *client) removeMapping(id string) {
	c.mappingMu.Lock()
	out := c.mappings[:0]
	for _, m := range c.mappings {
		if m.ID != id {
			out = append(out, m)
		}
	}
	c.mappings = out
	c.mappingMu.Unlock()
}

// desiredProto 按 relay 服务列表返回某映射应使用的本地监听协议。
//
// 列表里没有该服务时按 TCP 占位（known=false，调用方记 WARN）；
// 未知协议同样按 TCP 处理。与 startListeners 的既有语义一致。
func (c *client) desiredProto(serviceID string) (string, bool) {
	services, _ := c.servicesView()
	info, ok := services[serviceID]
	if !ok {
		return "tcp", false
	}
	switch strings.ToLower(strings.TrimSpace(info.Proto)) {
	case "udp":
		return "udp", true
	case "", "tcp":
		return "tcp", true
	default:
		return "tcp", true
	}
}

// validateMappingForm 校验本地映射的 service_id 与监听地址。
//
// 规则：ID 与 [client] service.<ID> 一致（a-z0-9-_，1..64），不接受 default
// （与安全边界一致）；地址允许裸端口/":port"/host:port，端口 1..65535，
// 总长度 <= 256。
func validateMappingForm(id, listen string) (string, string, error) {
	id = strings.TrimSpace(id)
	listen = strings.TrimSpace(listen)
	if len(id) > maxMappingFieldLen || len(listen) > maxMappingFieldLen {
		return "", "", fmt.Errorf("字段长度超过 %d 字节", maxMappingFieldLen)
	}
	if err := config.ValidateServiceID(id); err != nil {
		return "", "", err
	}
	if strings.EqualFold(id, config.DefaultServiceID) {
		return "", "", fmt.Errorf("service_id default 不允许在页面上新增/修改")
	}
	addr, err := config.NormalizeListenAddr(listen)
	if err != nil {
		return "", "", fmt.Errorf("本地监听地址非法: %w", err)
	}
	return id, addr, nil
}

// saveMapping 新增或修改一个本地监听映射：运行时监听 → ini。
//
// service_id 已存在时按"修改"处理：地址变化则换监听（失败保留旧监听），
// 协议变化走 hotSwitchProto；成功后写回 ini，失败回滚。
func (c *client) saveMapping(id, listen string) error {
	id, addr, err := validateMappingForm(id, listen)
	if err != nil {
		return err
	}
	c.mappingOpMu.Lock()
	defer c.mappingOpMu.Unlock()
	if c.ctx.Err() != nil {
		return errors.New("client: 正在关停")
	}

	old, exists := c.mappingByID(id)
	if !exists {
		return c.addMapping(id, addr)
	}
	return c.updateMapping(old, addr)
}

// addMapping 新增监听并写回 ini。
func (c *client) addMapping(id, addr string) error {
	proto, known := c.desiredProto(id)
	if !known {
		c.log.Warn("relay 服务列表中不存在该服务，按 TCP 打开本地监听；接入请求会被 relay 拒绝",
			"event", "service_missing", "service_id", id, "detail", "listen="+addr)
	}

	l, err := c.newListener(proto, addr)
	if err != nil {
		return fmt.Errorf("打开本地监听失败: %w", err)
	}
	if c.ctx.Err() != nil {
		l.close()
		return errors.New("client: 正在关停")
	}
	l.svcID = id
	l.listen = addr

	c.setMapping(config.ClientService{ID: id, Listen: addr})
	c.addListener(l)
	c.serveListener(l)

	if err := config.PatchValue(c.cfg.Path, "client", "service."+id, addr); err != nil {
		l.close()
		c.removeListener(l)
		c.removeMapping(id)
		return fmt.Errorf("监听已打开，但写回 ini 失败，已回滚: %w", err)
	}
	c.log.Info("管理页新增本地映射", "event", "admin_action", "action", "client-mapping-save",
		"service_id", id, "proto", proto, "old", "", "new", addr)
	return nil
}

// updateMapping 修改已有映射的监听地址/协议并写回 ini。
func (c *client) updateMapping(old config.ClientService, addr string) error {
	wantProto, known := c.desiredProto(old.ID)
	if !known {
		c.log.Warn("relay 服务列表中不存在该服务，按 TCP 打开本地监听；接入请求会被 relay 拒绝",
			"event", "service_missing", "service_id", old.ID, "detail", "listen="+addr)
	}

	if addr == old.Listen {
		return c.updateMappingProtoOnly(old, wantProto)
	}

	// 地址变化：先开新监听，成功后再关旧监听（失败时旧监听原样保留）。
	c.listenSwitchMu.Lock()
	oldListener := c.listenerByService(old.ID)
	oldProto := wantProto
	if oldListener != nil {
		oldProto = oldListener.proto
	}
	nl, err := c.newListener(wantProto, addr)
	if err != nil {
		c.listenSwitchMu.Unlock()
		return fmt.Errorf("打开新监听失败（旧监听保持不变）: %w", err)
	}
	if c.ctx.Err() != nil {
		nl.close()
		c.listenSwitchMu.Unlock()
		return errors.New("client: 正在关停")
	}
	nl.svcID = old.ID
	nl.listen = addr
	if oldListener != nil {
		oldListener.close()
		c.replaceListener(oldListener, nl)
	} else {
		c.addListener(nl)
	}
	c.serveListener(nl)
	c.listenSwitchMu.Unlock()

	c.setMapping(config.ClientService{ID: old.ID, Listen: addr})
	if err := config.PatchValue(c.cfg.Path, "client", "service."+old.ID, addr); err != nil {
		// 回滚：关新监听、按旧地址/旧协议重开、恢复映射。
		c.listenSwitchMu.Lock()
		nl.close()
		if oldListener != nil {
			rl, rerr := c.newListener(oldProto, reopenAddr(oldListener))
			if rerr == nil {
				rl.svcID = old.ID
				rl.listen = oldListener.listen
				c.replaceListener(nl, rl)
				c.serveListener(rl)
			} else {
				c.removeListener(nl)
				c.log.Warn("映射写盘失败后回滚旧监听也失败", "event", "admin_action_rollback",
					"action", "client-mapping-save", "service_id", old.ID, "err", rerr)
			}
		} else {
			c.removeListener(nl)
		}
		c.listenSwitchMu.Unlock()
		c.setMapping(old)
		return fmt.Errorf("监听已切换，但写回 ini 失败，已回滚: %w", err)
	}
	c.log.Info("管理页修改本地映射", "event", "admin_action", "action", "client-mapping-update",
		"service_id", old.ID, "proto", wantProto, "old", old.Listen, "new", addr)
	return nil
}

// updateMappingProtoOnly 处理地址不变、仅协议可能变化的修改。
func (c *client) updateMappingProtoOnly(old config.ClientService, wantProto string) error {
	cur := c.listenerByService(old.ID)
	oldProto := ""
	if cur != nil {
		oldProto = cur.proto
	}
	changed := oldProto != "" && oldProto != wantProto
	if changed {
		c.hotSwitchProto(old.ID, wantProto)
		if c.listenerProto(old.ID) != wantProto {
			return fmt.Errorf("本地监听协议切换失败（已保留 %s）", oldProto)
		}
	}

	if err := config.PatchValue(c.cfg.Path, "client", "service."+old.ID, old.Listen); err != nil {
		if changed {
			c.hotSwitchProto(old.ID, oldProto)
		}
		return fmt.Errorf("写回 ini 失败，已回滚: %w", err)
	}
	c.log.Info("管理页修改本地映射", "event", "admin_action", "action", "client-mapping-update",
		"service_id", old.ID, "proto", wantProto, "old", old.Listen, "new", old.Listen)
	return nil
}

// deleteMapping 关闭本地监听并删除 ini 中的 service.<ID>。
func (c *client) deleteMapping(id string) error {
	id = strings.TrimSpace(id)
	if len(id) > maxMappingFieldLen {
		return fmt.Errorf("字段长度超过 %d 字节", maxMappingFieldLen)
	}
	if err := config.ValidateServiceID(id); err != nil {
		return err
	}
	if strings.EqualFold(id, config.DefaultServiceID) {
		return fmt.Errorf("service_id default 不允许在页面上删除")
	}
	c.mappingOpMu.Lock()
	defer c.mappingOpMu.Unlock()
	if c.ctx.Err() != nil {
		return errors.New("client: 正在关停")
	}

	old, exists := c.mappingByID(id)
	if !exists {
		return fmt.Errorf("映射 %s 不存在", id)
	}
	wantProto, _ := c.desiredProto(id)

	c.listenSwitchMu.Lock()
	l := c.listenerByService(id)
	oldProto := wantProto
	if l != nil {
		oldProto = l.proto
		l.close()
		c.removeListener(l)
	}
	c.listenSwitchMu.Unlock()
	c.removeMapping(id)

	if err := config.DeleteKey(c.cfg.Path, "client", "service."+id); err != nil {
		// 回滚：恢复映射并重开监听。
		c.setMapping(old)
		rl, rerr := c.newListener(oldProto, old.Listen)
		if rerr == nil {
			rl.svcID = id
			rl.listen = old.Listen
			c.addListener(rl)
			c.serveListener(rl)
		} else {
			c.log.Warn("删除映射写盘失败后重开监听也失败", "event", "admin_action_rollback",
				"action", "client-mapping-delete", "service_id", id, "err", rerr)
		}
		return fmt.Errorf("监听已关闭，但写回 ini 失败，已尝试回滚: %w", err)
	}
	c.log.Info("管理页删除本地映射", "event", "admin_action", "action", "client-mapping-delete",
		"service_id", id, "old", old.Listen, "new", "")
	return nil
}

// mappingView 是管理页「本地端口映射表」的一行（阶段 7：映射 + 运行期协议）。
type mappingView struct {
	ServiceID string
	Listen    string
	Proto     string
	Actual    string
}

// clientMappingForms 是映射行上的写表单数据；Show=false 时只显示“只读”。
type clientMappingForms struct {
	Show        bool
	SaveToken   string
	DeleteToken string
}

// clientMappingRow 是「本地端口映射表」表格一行的渲染数据。
//
// service_id / 监听地址 / 实际绑定都是普通 string（外部可控值也只以 string
// 传入），由 html/template 转义；这里不携带任何 template.HTML。
type clientMappingRow struct {
	ServiceID string
	Listen    string
	Actual    string
	Proto     string
	Forms     clientMappingForms
}

// clientMappingRows 装配映射表行（含每行的修改/删除表单与一次性 CSRF 令牌）。
//
// 主服务（default）在表格里只读：与写 Action 的白名单一致。
func (c *client) clientMappingRows() []clientMappingRow {
	views := c.mappingViews()
	writable := c.adminWritable()
	rows := make([]clientMappingRow, 0, len(views))
	for _, m := range views {
		row := clientMappingRow{
			ServiceID: m.ServiceID,
			Listen:    m.Listen,
			Actual:    orDash(m.Actual),
			Proto:     m.Proto,
		}
		if writable && m.ServiceID != config.DefaultServiceID {
			row.Forms = clientMappingForms{
				Show:        true,
				SaveToken:   c.csrfToken(),
				DeleteToken: c.csrfToken(),
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// mappingViews 返回映射表视图。
//
// 以**已打开的本地监听**为准而不是映射表：启动阶段映射表先于监听建立，
// 若页面过早显示映射，外部观察者会误以为端口已就绪。动态新增/修改/删除
// 都同时维护映射表与监听，因此两者在稳定状态下一致。
func (c *client) mappingViews() []mappingView {
	c.listenerMu.Lock()
	listeners := append([]*localListener(nil), c.listeners...)
	c.listenerMu.Unlock()

	out := make([]mappingView, 0, len(listeners))
	for _, l := range listeners {
		listen := l.listen
		if m, ok := c.mappingByID(l.svcID); ok {
			listen = m.Listen
		}
		out = append(out, mappingView{
			ServiceID: l.svcID,
			Listen:    listen,
			Proto:     l.proto,
			Actual:    l.actualAddr(),
		})
	}
	return out
}
