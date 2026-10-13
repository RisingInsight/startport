// keys.go 「agent 接入密钥」的面板端点。
//
// 权限边界：全部端点走面板 withAuth（仅管理密钥 api_key）——agent 密钥
// 只能打 /v1/*，永远摸不到这组管理端点。全钥只在 create 响应里出现一次，
// list/改名/禁用/删除一律不含摘要与全钥。
package panel

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/RisingInsight/startport/internal/apikeys"
)

func (p *Panel) keysStore() *apikeys.APIKeys { return p.cfg.APIKeys }

// keysList GET /startport/api/keys → {keys: [...]}（无摘要、无全钥）。
func (p *Panel) keysList(w http.ResponseWriter, r *http.Request) {
	st := p.keysStore()
	if st == nil {
		writeErr(w, http.StatusNotImplemented, "api keys not enabled")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": st.List(), "hidden_names": st.HiddenNames()})
}

// keysSetHidden POST /startport/api/keys/hidden {name, hidden}。
// 控制「分组用量·按密钥」里该名称历史分组的显示/隐藏(纯显示偏好,桶数据不动;
// name 空串 = 管理密钥组)。
func (p *Panel) keysSetHidden(w http.ResponseWriter, r *http.Request) {
	st := p.keysStore()
	if st == nil {
		writeErr(w, http.StatusNotImplemented, "api keys not enabled")
		return
	}
	var body struct {
		Name   string `json:"name"`
		Hidden bool   `json:"hidden"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json body")
		return
	}
	st.SetHidden(body.Name, body.Hidden)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// keysCreate POST /startport/api/keys {name} → 全钥（仅此一次）+ 对外视图。
func (p *Panel) keysCreate(w http.ResponseWriter, r *http.Request) {
	st := p.keysStore()
	if st == nil {
		writeErr(w, http.StatusNotImplemented, "api keys not enabled")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json body")
		return
	}
	full, k, err := st.Create(body.Name)
	if err != nil {
		switch {
		case errors.Is(err, apikeys.ErrBadName):
			writeErr(w, http.StatusBadRequest, "名称须为 1-40 个字符")
		case errors.Is(err, apikeys.ErrLimit):
			writeErr(w, http.StatusBadRequest, "密钥数量已达上限（50），请先删除不用的密钥")
		default:
			writeErr(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	// 全钥仅出现在本响应；日志只记名称与前缀。
	log.Printf("panel: 创建 agent 密钥 %q（%s…）", k.Name, k.Prefix)
	writeJSON(w, http.StatusOK, map[string]any{"key": k, "full": full})
}

// keysRename POST /startport/api/keys/{id}/rename {name}。
func (p *Panel) keysRename(w http.ResponseWriter, r *http.Request) {
	st := p.keysStore()
	if st == nil {
		writeErr(w, http.StatusNotImplemented, "api keys not enabled")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json body")
		return
	}
	k, err := st.Rename(r.PathValue("id"), body.Name)
	if err != nil {
		writeKeysErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": k})
}

// keysSetDisabled POST /startport/api/keys/{id}/disable {disabled}。
// 禁用立即生效（/v1 校验侧跳过禁用行），行保留供重新启用。
func (p *Panel) keysSetDisabled(w http.ResponseWriter, r *http.Request) {
	st := p.keysStore()
	if st == nil {
		writeErr(w, http.StatusNotImplemented, "api keys not enabled")
		return
	}
	var body struct {
		Disabled bool `json:"disabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json body")
		return
	}
	k, err := st.SetDisabled(r.PathValue("id"), body.Disabled)
	if err != nil {
		writeKeysErr(w, err)
		return
	}
	log.Printf("panel: agent 密钥 %q 禁用=%v", k.Name, body.Disabled)
	writeJSON(w, http.StatusOK, map[string]any{"key": k})
}

// keysDelete POST /startport/api/keys/{id}/delete → 立即失效。
func (p *Panel) keysDelete(w http.ResponseWriter, r *http.Request) {
	st := p.keysStore()
	if st == nil {
		writeErr(w, http.StatusNotImplemented, "api keys not enabled")
		return
	}
	if err := st.Delete(r.PathValue("id")); err != nil {
		writeKeysErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func writeKeysErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, apikeys.ErrNotFound):
		writeErr(w, http.StatusNotFound, "key not found")
	case errors.Is(err, apikeys.ErrBadName):
		writeErr(w, http.StatusBadRequest, "名称须为 1-40 个字符")
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}
