package agent

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/lsong98sh/pairlink/internal/admin"
	"github.com/lsong98sh/pairlink/internal/config"
)

// handleServiceSave 处理"新增/修改服务"Action。
func (a *agent) handleServiceSave(w http.ResponseWriter, r *http.Request) {
	if !admin.OnlyPostFields(r, "service_id", "proto", "local") {
		http.Error(w, "400 表单字段非法", http.StatusBadRequest)
		return
	}
	id := strings.TrimSpace(r.PostFormValue("service_id"))
	proto := strings.TrimSpace(r.PostFormValue("proto"))
	local := strings.TrimSpace(r.PostFormValue("local"))
	old, existed := a.svcStore.get(id)
	oldText := ""
	if existed {
		oldText = old.Proto + ":" + old.Addr
	}
	if err := a.saveService(id, proto, local); err != nil {
		if a.adminSrv != nil {
			a.adminSrv.AuditFailure(r, "agent-service-save", "service."+id, err)
		}
		admin.RedirectNotice(w, r, "/status", true, err.Error())
		return
	}
	newText := proto + ":" + local
	if svc, ok := a.svcStore.get(id); ok {
		newText = svc.Proto + ":" + svc.Addr
	}
	if a.adminSrv != nil {
		a.adminSrv.Audit(r, "agent-service-save", "service."+id, oldText, newText)
	}
	admin.RedirectNotice(w, r, "/status", false, fmt.Sprintf("服务 %s 已保存", id))
}

// handleServiceDelete 处理"删除服务"Action。
func (a *agent) handleServiceDelete(w http.ResponseWriter, r *http.Request) {
	if !admin.OnlyPostFields(r, "service_id") {
		http.Error(w, "400 表单字段非法", http.StatusBadRequest)
		return
	}
	id := strings.TrimSpace(r.PostFormValue("service_id"))
	old, existed := a.svcStore.get(id)
	oldText := ""
	if existed {
		oldText = old.Proto + ":" + old.Addr
	}
	if err := a.deleteService(id); err != nil {
		if a.adminSrv != nil {
			a.adminSrv.AuditFailure(r, "agent-service-delete", "service."+id, err)
		}
		admin.RedirectNotice(w, r, "/status", true, err.Error())
		return
	}
	if a.adminSrv != nil {
		a.adminSrv.Audit(r, "agent-service-delete", "service."+id, oldText, "")
	}
	admin.RedirectNotice(w, r, "/status", false, fmt.Sprintf("服务 %s 已删除", id))
}

// handleMainSave 处理"修改主服务"Action。
func (a *agent) handleMainSave(w http.ResponseWriter, r *http.Request) {
	if !admin.OnlyPostFields(r, "protocol", "local") {
		http.Error(w, "400 表单字段非法", http.StatusBadRequest)
		return
	}
	proto := strings.TrimSpace(r.PostFormValue("protocol"))
	local := strings.TrimSpace(r.PostFormValue("local"))
	old, existed := a.svcStore.get(config.DefaultServiceID)
	oldText := ""
	if existed {
		oldText = old.Proto + ":" + old.Addr
	}
	if err := a.saveMainService(proto, local); err != nil {
		if a.adminSrv != nil {
			a.adminSrv.AuditFailure(r, "agent-main-save", "server.protocol/local", err)
		}
		admin.RedirectNotice(w, r, "/status", true, err.Error())
		return
	}
	newText := proto + ":" + local
	if svc, ok := a.svcStore.get(config.DefaultServiceID); ok {
		newText = svc.Proto + ":" + svc.Addr
	}
	if a.adminSrv != nil {
		a.adminSrv.Audit(r, "agent-main-save", "server.protocol/local", oldText, newText)
	}
	admin.RedirectNotice(w, r, "/status", false, "主服务已更新")
}
