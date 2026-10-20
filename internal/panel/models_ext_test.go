package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/RisingInsight/startport/internal/extchan"
)

func extModelsServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/algo/api/v2/model/list" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"chat":[
			{"key":"qmodel_38max","display_name":"Qwen3.8-Max","is_reasoning":true,"is_vl":true,
			 "price_factor":0.5,"max_input_tokens":200000,
			 "thinking_config":{"enabled":{"efforts":{"low":{},"medium":{},"xhigh":{}}}}},
			{"key":"dmodel","display_name":"DeepSeek-V4-Pro","is_reasoning":true,"is_vl":false,"price_factor":0.8}
		]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestPanelModelsIncludesExtChannels 纯外部渠道部署（无 CodeBuddy 账号）也能在
// 「模型与档位」看到 ext: 模型，并带平台/协议/适配器与重名标记。
func TestPanelModelsIncludesExtChannels(t *testing.T) {
	qoderSrv := extModelsServer(t)
	mgr := extchan.New(extchan.Config{TimeoutSeconds: 5})
	mgr.ReplaceAll([]extchan.ChannelDef{
		{
			Name: "qoder", BaseURL: qoderSrv.URL, Protocol: "openai", Adapter: "qoder",
			Models: []string{"Qwen3.8-Max"},
			Credentials: []extchan.CredentialDef{
				{Label: "acc-q", Secret: `{"accessToken":"jwt","userId":"u"}`},
			},
		},
		{
			Name: "trae", BaseURL: "https://trae-api-cn.mchost.guru", Protocol: "openai", Adapter: "trae",
			Models: []string{"Qwen3.8-Max", "glm-5.2"},
			Credentials: []extchan.CredentialDef{
				{Label: "acc-t", Secret: `{"accessToken":"jwt"}`},
			},
		},
	})
	p := New(Config{Version: "test", APIKey: "k", ExtChan: mgr})
	req := httptest.NewRequest(http.MethodGet, "/startport/api/models", nil)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Models []struct {
			ID               string   `json:"id"`
			Source           string   `json:"source"`
			Provider         string   `json:"provider"`
			PlatformName     string   `json:"platform_name"`
			Protocol         string   `json:"protocol"`
			Adapter          string   `json:"adapter"`
			NameCollision    bool     `json:"name_collision"`
			Credits          string   `json:"credits"`
			ContextLength    int      `json:"context_length"`
			MaxOutputTokens  int      `json:"max_output_tokens"`
			SupportedEfforts []string `json:"supported_efforts"`
			SupportsImages   bool     `json:"supports_images"`
			SupportsTools    bool     `json:"supports_tool_call"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	byID := map[string]int{}
	for i, m := range out.Models {
		byID[m.ID] = i
	}
	qi, ok := byID["ext:qoder/Qwen3.8-Max"]
	if !ok {
		t.Fatalf("缺 qoder 模型: %v", byID)
	}
	q := out.Models[qi]
	if q.Source != "ext" || q.Provider != "qoder" || q.PlatformName != "Qoder 国内" ||
		q.Protocol != "openai" || q.Adapter != "qoder" || !q.NameCollision {
		t.Errorf("qoder 条目异常: %+v", q)
	}
	if q.Credits != "x0.5 credits" || q.ContextLength != 200000 || q.MaxOutputTokens != 32768 ||
		len(q.SupportedEfforts) != 3 || !q.SupportsImages || !q.SupportsTools {
		t.Errorf("qoder 元数据异常: %+v", q)
	}
	if _, ok := byID["ext:trae/glm-5.2"]; !ok {
		t.Errorf("缺 trae 模型: %v", byID)
	}
	if ti, ok := byID["ext:trae/Qwen3.8-Max"]; ok {
		q2 := out.Models[ti]
		if q2.PlatformName == "" || !q2.NameCollision {
			t.Errorf("同名模型应标记 collision: %+v", q2)
		}
	} else {
		t.Errorf("缺 trae 同名模型: %v", byID)
	}
}
