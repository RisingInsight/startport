// qodercosy_test.go COSY 签名与编码的确定性 fixture 测试（v1.5.0）。
//
// fixture 对拍基准：参考实现 `cosy.rs` 自述已与 Node crypto 逐字节对拍
// （cosy.rs:29-30）；本文件的期望值用同一算法在 Node 下生成（AES-128-CBC /
// MD5 / base64 都是标准算法），实现必须与它们逐字节一致。随机材料全部以参数
// 注入（AES key / requestId / 时间戳 / cosyKey），生产路径才走 crypto/rand。
// 真实上游联调仍需用户实测——fixture 证明"与参考实现同形"，不证明上游接受。
package provider

import (
	"encoding/base64"
	"strings"
	"testing"
)

// 固定材料（与 Node 对拍脚本一致）。
const (
	fixtureAESKey  = "0123456789abcdef"
	fixtureUID     = "u-1"
	fixtureToken   = "tok-1"
	fixtureName    = "Alice"
	fixtureEmail   = "a@b.c"
	fixtureReqID   = "00000000-0000-4000-8000-000000000000"
	fixtureTS      = "1770000000"
	fixtureBody    = "SIGNBODY"
	fixtureSigPath = "/api/v2/service/pro/sse/agent_chat_generation"
	fixtureCosyKey = "FIXEDCOSYKEYSIGNATURE=="
)

// 身份 JSON：键序手写（uid / security_oauth_token / name / aid / email）。
func TestQoderIdentityJSONKeyOrder(t *testing.T) {
	got := cosyBuildIdentityJSON(CosyIdentity{
		UserID: fixtureUID, AuthToken: fixtureToken, Name: fixtureName, Email: fixtureEmail,
	})
	want := `{"uid":"u-1","security_oauth_token":"tok-1","name":"Alice","aid":"","email":"a@b.c"}`
	if got != want {
		t.Errorf("身份 JSON 键序/形态异常:\n got %s\nwant %s", got, want)
	}
	if len(got) != 84 {
		t.Errorf("身份 JSON 长度 = %d, want 84（对拍基准）", len(got))
	}
}

// AES-128-CBC（key==iv）+ PKCS#7 → base64：与 Node crypto 逐字节一致。
func TestQoderAesInfoFixture(t *testing.T) {
	got, err := cosyAesInfoBase64(fixtureAESKey,
		cosyBuildIdentityJSON(CosyIdentity{
			UserID: fixtureUID, AuthToken: fixtureToken, Name: fixtureName, Email: fixtureEmail,
		}))
	if err != nil {
		t.Fatalf("AES: %v", err)
	}
	want := "+DGTa5lPw8l0y/64Vpmyj+B0DDaUVR0UYbc7ASAqfO8ciepVWoWbP7CSyJxecJ8/l3GO7IWsnsPRCc7EcPAS/TiKOdGchZBCMHVjiOkFIFjtRJ/l1ZmGr/05osvjX7Gb"
	if got != want {
		t.Errorf("info 不一致:\n got %s\nwant %s", got, want)
	}
}

// payload：手写键序（version/requestId/info/cosyVersion/ideVersion）→ 标准 base64。
func TestQoderPayloadFixture(t *testing.T) {
	info := "+DGTa5lPw8l0y/64Vpmyj+B0DDaUVR0UYbc7ASAqfO8ciepVWoWbP7CSyJxecJ8/l3GO7IWsnsPRCc7EcPAS/TiKOdGchZBCMHVjiOkFIFjtRJ/l1ZmGr/05osvjX7Gb"
	got := base64.StdEncoding.EncodeToString([]byte(cosyBuildPayloadJSON(info, fixtureReqID)))
	want := "eyJ2ZXJzaW9uIjoidjEiLCJyZXF1ZXN0SWQiOiIwMDAwMDAwMC0wMDAwLTQwMDAtODAwMC0wMDAwMDAwMDAwMDAiLCJpbmZvIjoiK0RHVGE1bFB3OGwweS82NFZwbXlqK0IwRERhVVZSMFVZYmM3QVNBcWZPOGNpZXBWV29XYlA3Q1N5SnhlY0o4L2wzR083SVdzbnNQUkNjN0VjUEFTL1RpS09kR2NoWkJDTUhWamlPa0ZJRmp0UkovbDFabUdyLzA1b3N2alg3R2IiLCJjb3N5VmVyc2lvbiI6IjEuMS4zOCIsImlkZVZlcnNpb24iOiIifQ=="
	if got != want {
		t.Errorf("payload 不一致:\n got %s\nwant %s", got, want)
	}
}

// 签名：MD5(payload \n key \n 时间戳 \n body \n sig_path)，hex 小写。
func TestQoderSignatureFixture(t *testing.T) {
	payload := base64.StdEncoding.EncodeToString([]byte(cosyBuildPayloadJSON(
		"+DGTa5lPw8l0y/64Vpmyj+B0DDaUVR0UYbc7ASAqfO8ciepVWoWbP7CSyJxecJ8/l3GO7IWsnsPRCc7EcPAS/TiKOdGchZBCMHVjiOkFIFjtRJ/l1ZmGr/05osvjX7Gb",
		fixtureReqID)))
	got := cosySignature(payload, fixtureCosyKey, fixtureTS, []byte(fixtureBody), fixtureSigPath)
	if got != "9bd3ea1b579d4823a7cdb87429b8d886" {
		t.Errorf("签名不一致: %s", got)
	}
	if got := cosyBodyHash([]byte(fixtureBody)); got != "7d46aee1158f49e7684a12bc3cb58a6b" {
		t.Errorf("body hash 不一致: %s", got)
	}
}

// encode_body：三步字节变换（base64 → 尾/中/首重排 → 字母表替换，'='→'$'）。
// 两条覆盖：len%3==0 与 len%3!=0（余数留中段）。
func TestQoderEncodeBodyFixtures(t *testing.T) {
	cases := map[string]string{
		`{"a":1}`:                           "ep$$BMn%mYKi",
		`{"model":"qfmodel","messages":[]}`: "j^#SJLNYByS..WgOuSWzDSf*GoKtDxmYKtu(CLuoByBE",
	}
	for in, want := range cases {
		got := string(qoderEncodeBody([]byte(in)))
		if got != want {
			t.Errorf("encode_body(%s):\n got %q\nwant %q", in, got, want)
		}
	}
}

// 编码后不含标准 base64 的填充符 '='（已换成 '$'）。
func TestQoderEncodeBodyNoStdPadding(t *testing.T) {
	out := string(qoderEncodeBody([]byte(`{"pad":"x"}`)))
	if strings.Contains(out, "=") {
		t.Errorf("编码后仍有 '=': %s", out)
	}
}

// 签名路径：去掉 /algo 前缀、不含查询串。
func TestQoderSignaturePath(t *testing.T) {
	got, err := CosySignaturePath(
		"https://gateway.qoder.com.cn/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=1&Encode=1")
	if err != nil {
		t.Fatalf("SignaturePath: %v", err)
	}
	want := "/api/v2/service/pro/sse/agent_chat_generation"
	if got != want {
		t.Errorf("sig path = %q, want %q", got, want)
	}
	// 不带 /algo 前缀的路径原样保留。
	got, _ = CosySignaturePath("https://x.example/other/path")
	if got != "/other/path" {
		t.Errorf("sig path = %q", got)
	}
}

// RSA：PKCS#1 v1.5，输出 128 字节（1024 位模数）、同明文两次密文不同（PS 随机，
// 固定 PS 会退化成确定性加密——可被指纹化的特征，cosy.rs:316-318）。
func TestQoderRSAEncrypt(t *testing.T) {
	a, err := cosyRSAEncryptBase64(fixtureAESKey)
	if err != nil {
		t.Fatalf("RSA: %v", err)
	}
	b, err := cosyRSAEncryptBase64(fixtureAESKey)
	if err != nil {
		t.Fatalf("RSA#2: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(a)
	if err != nil {
		t.Fatalf("密文不是合法 base64: %v", err)
	}
	if len(raw) != 128 {
		t.Errorf("RSA 密文长度 = %d, want 128", len(raw))
	}
	if a == b {
		t.Error("同明文两次加密结果相同（PS 未随机化，可被指纹化）")
	}
}

// BuildAuthHeaders：头集合全量（认证头形态 / Cosy-* / X-Request-Id），
// 注入固定材料后 Authorization 的 payload 段与 fixture 一致。
func TestQoderBuildAuthHeaders(t *testing.T) {
	headers, err := BuildAuthHeaders([]byte(fixtureBody),
		"https://gateway.qoder.com.cn/algo/api/v2/service/pro/sse/agent_chat_generation",
		CosyIdentity{UserID: fixtureUID, AuthToken: fixtureToken, Name: fixtureName,
			Email: fixtureEmail, MachineID: "mach-1"},
		&CosyMaterial{AESKey: fixtureAESKey, RequestID: fixtureReqID, Timestamp: fixtureTS})
	if err != nil {
		t.Fatalf("BuildAuthHeaders: %v", err)
	}
	byName := map[string]string{}
	for _, h := range headers {
		byName[h[0]] = h[1]
	}
	auth := byName["Authorization"]
	if !strings.HasPrefix(auth, "Bearer COSY.") {
		t.Errorf("Authorization 形态异常: %q", auth)
	}
	parts := strings.Split(strings.TrimPrefix(auth, "Bearer COSY."), ".")
	if len(parts) != 2 {
		t.Fatalf("COSY 载荷应为 payload.signature 两段: %q", auth)
	}
	if !strings.HasPrefix(parts[0], "eyJ2ZXJzaW9uIjoidjEi") {
		t.Errorf("payload 不是期望的键序: %q", parts[0])
	}
	if byName["Cosy-Version"] != "1.1.38" || byName["Cosy-Clienttype"] != "5" ||
		byName["Cosy-Clientip"] != "127.0.0.1" || byName["Cosy-Data-Policy"] != "disagree" ||
		byName["Login-Version"] != "v2" || byName["Cosy-Machinetype"] != "5" {
		t.Errorf("Cosy-* 常量头异常: %+v", byName)
	}
	if byName["Cosy-Bodyhash"] != "7d46aee1158f49e7684a12bc3cb58a6b" {
		t.Errorf("Cosy-Bodyhash = %q", byName["Cosy-Bodyhash"])
	}
	if byName["Cosy-Bodylength"] != "8" {
		t.Errorf("Cosy-Bodylength = %q (body=%q)", byName["Cosy-Bodylength"], fixtureBody)
	}
	if byName["Cosy-Sigpath"] != fixtureSigPath {
		t.Errorf("Cosy-Sigpath = %q", byName["Cosy-Sigpath"])
	}
	if byName["X-Request-Id"] != fixtureReqID {
		t.Errorf("X-Request-Id = %q", byName["X-Request-Id"])
	}
	if byName["Cosy-User"] != fixtureUID || byName["Cosy-Machineid"] != "mach-1" {
		t.Errorf("身份头异常: %+v", byName)
	}
}

// 缺 userId / token：装配期即报（不带病签名）。
func TestQoderBuildAuthHeadersRejectsIncompleteIdentity(t *testing.T) {
	if _, err := BuildAuthHeaders(nil, "https://x/algo/p", CosyIdentity{AuthToken: "t"}, nil); err == nil {
		t.Error("缺 userId 应报错")
	}
	if _, err := BuildAuthHeaders(nil, "https://x/algo/p", CosyIdentity{UserID: "u"}, nil); err == nil {
		t.Error("缺 token 应报错")
	}
}
