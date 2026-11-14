package client

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/lsong98sh/pairlink/internal/admin"
)

// handleMappingSave 处理"新增/修改映射"Action。
func (c *client) handleMappingSave(w http.ResponseWriter, r *http.Request) {
	if !admin.OnlyPostFields(r, "service_id", "listen") {
		http.Error(w, "400 表单字段非法", http.StatusBadRequest)
		return
	}
	id := strings.TrimSpace(r.PostFormValue("service_id"))
	listen := strings.TrimSpace(r.PostFormValue("listen"))
	old, existed := c.mappingByID(id)
	oldText := ""
	if existed {
		oldText = old.Listen
	}
	if err := c.saveMapping(id, listen); err != nil {
		if c.admin != nil {
			c.admin.AuditFailure(r, "client-mapping-save", "service."+id, err)
		}
		admin.RedirectNotice(w, r, "/client", true, err.Error())
		return
	}
	newText := listen
	if m, ok := c.mappingByID(id); ok {
		newText = m.Listen
	}
	if c.admin != nil {
		c.admin.Audit(r, "client-mapping-save", "service."+id, oldText, newText)
	}
	admin.RedirectNotice(w, r, "/client", false, fmt.Sprintf("映射 %s 已保存", id))
}

// handleMappingDelete 处理"删除映射"Action。
func (c *client) handleMappingDelete(w http.ResponseWriter, r *http.Request) {
	if !admin.OnlyPostFields(r, "service_id") {
		http.Error(w, "400 表单字段非法", http.StatusBadRequest)
		return
	}
	id := strings.TrimSpace(r.PostFormValue("service_id"))
	old, existed := c.mappingByID(id)
	oldText := ""
	if existed {
		oldText = old.Listen
	}
	if err := c.deleteMapping(id); err != nil {
		if c.admin != nil {
			c.admin.AuditFailure(r, "client-mapping-delete", "service."+id, err)
		}
		admin.RedirectNotice(w, r, "/client", true, err.Error())
		return
	}
	if c.admin != nil {
		c.admin.Audit(r, "client-mapping-delete", "service."+id, oldText, "")
	}
	admin.RedirectNotice(w, r, "/client", false, fmt.Sprintf("映射 %s 已删除", id))
}
