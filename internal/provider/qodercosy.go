// qodercosy.go Qoder COSY 请求签名与请求体编码（v1.5.0）。
//
// 移植依据：`C:/tmp/ref/qoder/cosy.rs`（自述已与 Node crypto 逐字节对拍，
// cosy.rs:29-30）；fixture 测试以该行为为基准（见 qodercosy_test.go）。
//
// ── 为什么不能只发 Bearer ───────────────────────────────────
// Qoder 的推理网关（/algo/... 一族）不认普通 Bearer，而是一套自签名：
//  1. 组装用户身份 JSON（键序手写：uid / security_oauth_token / name / aid / email）；
//  2. 一次性 AES-128-CBC 密钥加密身份 JSON → `info`（**key 与 iv 是同一段
//     16 字节**，源实现既有做法，cosy.rs:276-282）；
//  3. 内置 RSA 公钥（PKCS#1 v1.5）加密该 AES 密钥 → `key`（PS 非零随机）；
//  4. {version, requestId, info, cosyVersion, ideVersion} 手写键序后 base64 →
//     `payload`；
//  5. MD5(payload \n key \n 时间戳秒 \n 编码后请求体 \n 签名路径) → 签名；
//  6. `Authorization: Bearer COSY.<payload>.<签名>` + 一组 Cosy-* 头。
//
// ── 两条顺序红线 ────────────────────────────────────────────
//   - **先 encode_body 再签名**：签名覆盖编码后的字节（cosy.rs:16-17）；
//   - 签名路径去掉 `/algo` 前缀、不含查询串（cosy.rs:252-257）。
//
// ── 键序纪律（文档 §2.2）────────────────────────────────────
// 身份 JSON 与 payload 的键序**手写字符串拼接**，不用 map/struct 序列化——
// 键序进密文进签名，序列化器的键序策略一变，对拍基准就没了。
package provider

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/url"
	"runtime"
	"strings"
	"time"
)

// COSY 常量（cosy.rs:52-82，随代码分发的非机密材料）。
const (
	qoderCosyVersion  = "1.1.38" // Cosy-Version / payload.cosyVersion
	qoderClientType   = "5"      // Cosy-Clienttype
	qoderMachineType  = "5"      // Cosy-Machinetype
	qoderDataPolicy   = "disagree"
	qoderLoginVersion = "v2"
	qoderClientIP     = "127.0.0.1" // Cosy-Clientip（源实现写死）
	qoderAuthPrefix   = "COSY."     // Authorization: Bearer COSY.<payload>.<sig>
)

// qoderRSAModulusHex COSY 身份加密用的 RSA 公钥模数（1024 位，hex）。
// 客户端内置固定公钥，只用于加密一次性 AES 密钥（cosy.rs:66-74）。
const qoderRSAModulusHex = "c0f22307e5cd362e296bb04470f6de8fbf935ce24e8fcf511a0e2701329769c4" +
	"a76e499bb938036a52af1eaf818cf79a2600620e3ce87e371d2ca6d85803606a1" +
	"b3fa5e874643c9ed2db7e85673ef7227fca56e2e7c08f0927609bb896a9f24be1" +
	"782099a66016a5bfdc3f1ff756bfc9e88d7b5dc5be30bf45a0223a00ebcecf"

// qoderCustomAlphabet / qoderStdAlphabet encode_body 的自定义字母表（cosy.rs:80-82）。
const (
	qoderCustomAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
	qoderStdAlphabet    = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
)

// CosyIdentity 一次签名所需的身份素材（cosy.rs:85-96）。
type CosyIdentity struct {
	UserID    string
	AuthToken string
	Name      string
	Email     string
	MachineID string
}

// CosyMaterial 签名的随机/时间材料（测试注入固定值；生产走 crypto/rand）。
type CosyMaterial struct {
	AESKey    string // 16 ASCII 字符
	RequestID string // UUID v4
	Timestamp string // unix 秒（字符串形态参与签名）
}

// newCosyMaterial 生产随机材料：AES key 取密码学随机串前 16 位（cosy.rs:117-121）。
func newCosyMaterial() (*CosyMaterial, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, &HTTPError{Status: 500, Code: "qoder_sign_error", Message: "无法生成 Qoder 签名密钥"}
	}
	key := base64.RawURLEncoding.EncodeToString(buf)
	return &CosyMaterial{
		AESKey:    key[:16],
		RequestID: newAutoClawRequestID(), // UUID v4（复用 AutoClaw 的生成器，同一形态）
		Timestamp: fmt.Sprintf("%d", time.Now().Unix()),
	}, nil
}

// BuildAuthHeaders 构造一次请求所需的全部 COSY 头（cosy.rs:103-201）。
//
// `body` 必须是**已编码**（qoderEncodeBody 之后）的字节；签名覆盖它，
// 顺序颠倒会得到签名不匹配的上游错误。mat 为 nil 时生成生产随机材料。
func BuildAuthHeaders(body []byte, requestURL string, id CosyIdentity, mat *CosyMaterial) ([][2]string, error) {
	if id.UserID == "" {
		return nil, &HTTPError{Status: 400, Code: "qoder_identity_missing", Message: "Qoder 账号缺少用户标识（userId），无法签名"}
	}
	if id.AuthToken == "" {
		return nil, &HTTPError{Status: 401, Code: "token_invalid", Message: "Qoder 账号缺少访问令牌，请重新粘贴凭证"}
	}
	if mat == nil {
		gen, err := newCosyMaterial()
		if err != nil {
			return nil, err
		}
		mat = gen
	}

	// ② 身份 JSON → AES-CBC → base64（info）。
	info, err := cosyAesInfoBase64(mat.AESKey, cosyBuildIdentityJSON(id))
	if err != nil {
		return nil, err
	}
	// ③ AES 密钥 → RSA PKCS#1 v1.5 → base64（key）。
	cosyKey, err := cosyRSAEncryptBase64(mat.AESKey)
	if err != nil {
		return nil, err
	}
	// ④ payload（手写键序）→ 标准 base64。
	payload := base64.StdEncoding.EncodeToString(
		[]byte(cosyBuildPayloadJSON(info, mat.RequestID)))

	sigPath, err := CosySignaturePath(requestURL)
	if err != nil {
		return nil, err
	}
	signature := cosySignature(payload, cosyKey, mat.Timestamp, body, sigPath)
	bodyHash := cosyBodyHash(body)

	headers := [][2]string{
		{"Authorization", "Bearer " + qoderAuthPrefix + payload + "." + signature},
		{"Cosy-Key", cosyKey},
		{"Cosy-User", id.UserID},
		{"Cosy-Date", mat.Timestamp},
		{"Cosy-Version", qoderCosyVersion},
		{"Cosy-Machineid", id.MachineID},
		{"Cosy-Machinetoken", id.MachineID},
		{"Cosy-Machinetype", qoderMachineType},
		{"Cosy-Machineos", qoderMachineOS()},
		{"Cosy-Clienttype", qoderClientType},
		{"Cosy-Clientip", qoderClientIP},
		{"Cosy-Bodyhash", bodyHash},
		{"Cosy-Bodylength", fmt.Sprintf("%d", len(body))},
		{"Cosy-Sigpath", sigPath},
		{"Cosy-Data-Policy", qoderDataPolicy},
		{"Cosy-Organization-Id", ""},
		{"Cosy-Organization-Tags", ""},
		{"Login-Version", qoderLoginVersion},
		{"X-Request-Id", mat.RequestID},
	}
	return headers, nil
}

// cosyBuildIdentityJSON 身份 JSON（**键序手写**，cosy.rs:134-140）：
// {"uid":…,"security_oauth_token":…,"name":…,"aid":"","email":…}。
// 各值用 JSON 字符串转义（值里含引号/中文也不会拼坏）。
func cosyBuildIdentityJSON(id CosyIdentity) string {
	return "{\"uid\":" + qoderJSONText(id.UserID) +
		",\"security_oauth_token\":" + qoderJSONText(id.AuthToken) +
		",\"name\":" + qoderJSONText(id.Name) +
		",\"aid\":\"\"" +
		",\"email\":" + qoderJSONText(id.Email) + "}"
}

// cosyBuildPayloadJSON payload（**键序手写**，cosy.rs:150-153）：
// {"version":"v1","requestId":…,"info":…,"cosyVersion":"1.1.38","ideVersion":""}。
// info 是 base64、requestId 是 UUID、版本是常量，都不需要转义。
func cosyBuildPayloadJSON(info, requestID string) string {
	return "{\"version\":\"v1\",\"requestId\":\"" + requestID +
		"\",\"info\":\"" + info +
		"\",\"cosyVersion\":\"" + qoderCosyVersion +
		"\",\"ideVersion\":\"\"}"
}

// qoderJSONText 一个字符串的 JSON 文本形态（带引号与转义；cosy.rs:247-249）。
func qoderJSONText(value string) string {
	out, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return string(out)
}

// qoderDeterministicJSON 确定性 JSON 字节（Go map 键序按字典序，可作指纹输入）。
func qoderDeterministicJSON(v any) ([]byte, bool) {
	out, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	return out, true
}

// cosySignature MD5(payload \n key \n 时间戳 \n body \n sig_path)，hex 小写
// （cosy.rs:159-170）。
func cosySignature(payload, cosyKey, timestamp string, body []byte, sigPath string) string {
	h := md5.New()
	h.Write([]byte(payload))
	h.Write([]byte("\n"))
	h.Write([]byte(cosyKey))
	h.Write([]byte("\n"))
	h.Write([]byte(timestamp))
	h.Write([]byte("\n"))
	h.Write(body)
	h.Write([]byte("\n"))
	h.Write([]byte(sigPath))
	return hex.EncodeToString(h.Sum(nil))
}

// cosyBodyHash MD5(编码后请求体)，hex（cosy.rs:172-174）。
func cosyBodyHash(body []byte) string {
	sum := md5.Sum(body)
	return hex.EncodeToString(sum[:])
}

// CosySignaturePath 参与签名的路径：去掉 `/algo` 前缀、不含查询串
// （cosy.rs:252-257）。
func CosySignaturePath(requestURL string) (string, error) {
	u, err := url.Parse(requestURL)
	if err != nil {
		return "", &HTTPError{Status: 500, Code: "qoder_sign_error", Message: "Qoder 请求地址无效"}
	}
	path := u.Path
	return strings.TrimPrefix(path, "/algo"), nil
}

// qoderMachineOS Cosy-Machineos：`{arch}_{platform}`（cosy.rs:260-270）。
func qoderMachineOS() string {
	arch := "x86_64"
	if strings.Contains(runtime.GOARCH, "arm64") || strings.Contains(runtime.GOARCH, "aarch64") {
		arch = "aarch64"
	}
	platform := "linux"
	switch runtime.GOOS {
	case "windows":
		platform = "windows"
	case "darwin":
		platform = "darwin"
	}
	return arch + "_" + platform
}

// cosyAesInfoBase64 AES-128-CBC + PKCS#7，返回 base64 密文。
// **key 与 iv 都取同一段 16 字节**（cosy.rs:276-282 的既有做法）。
func cosyAesInfoBase64(key16, plaintext string) (string, error) {
	if len(key16) != 16 {
		return "", &HTTPError{Status: 500, Code: "qoder_sign_error", Message: "Qoder 签名密钥长度无效"}
	}
	block, err := aes.NewCipher([]byte(key16))
	if err != nil {
		return "", &HTTPError{Status: 500, Code: "qoder_sign_error", Message: "无法初始化 Qoder 签名算法"}
	}
	data := []byte(plaintext)
	pad := 16 - len(data)%16 // PKCS#7：补 1..16 字节，值等于补位长度
	data = append(data, bytes.Repeat([]byte{byte(pad)}, pad)...)
	enc := cipher.NewCBCEncrypter(block, []byte(key16)) // iv == key（源实现语义）
	out := make([]byte, len(data))
	enc.CryptBlocks(out, data)
	return base64.StdEncoding.EncodeToString(out), nil
}

// cosyRSAEncryptBase64 RSA 公钥加密（PKCS#1 v1.5），返回 base64 密文。
// 标准库实现与源手写模幂等价（同为 00 02 PS 00 M，PS 非零随机）。
func cosyRSAEncryptBase64(plaintext string) (string, error) {
	modulus, ok := new(big.Int).SetString(qoderRSAModulusHex, 16)
	if !ok {
		return "", &HTTPError{Status: 500, Code: "qoder_sign_error", Message: "Qoder 签名公钥无效"}
	}
	pub := &rsa.PublicKey{N: modulus, E: 65537}
	cipherText, err := rsa.EncryptPKCS1v15(rand.Reader, pub, []byte(plaintext))
	if err != nil {
		return "", &HTTPError{Status: 500, Code: "qoder_sign_error", Message: "Qoder 签名内容过长"}
	}
	return base64.StdEncoding.EncodeToString(cipherText), nil
}

// qoderEncodeBody 请求体编码（cosy.rs:212-240，三步纯字节变换）：
//  1. 标准 base64；
//  2. 按「尾段 / 中段 / 首段」重排（各段长 ⌊n/3⌋，余数留中段）；
//  3. 逐字符字母表替换（含 '=' → '$'）。
//
// 签名算在编码之后的字节上。
func qoderEncodeBody(payload []byte) []byte {
	standard := []byte(base64.StdEncoding.EncodeToString(payload))
	length := len(standard)
	third := length / 3

	table := make([]byte, 256)
	for i := range table {
		table[i] = byte(i)
	}
	for i := 0; i < len(qoderStdAlphabet); i++ {
		table[qoderStdAlphabet[i]] = qoderCustomAlphabet[i]
	}
	table['='] = '$'

	out := make([]byte, 0, length)
	for i := length - third; i < length; i++ { // 尾段
		out = append(out, table[standard[i]])
	}
	for i := third; i < length-third; i++ { // 中段
		out = append(out, table[standard[i]])
	}
	for i := 0; i < third; i++ { // 首段
		out = append(out, table[standard[i]])
	}
	return out
}

// qoderRandomUUID UUID v4（machine.rs:20-25 同款）。
func qoderRandomUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("req-%016x", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// qoderSessionID 会话 id（protocol.rs:477-494）：sha256("qoder-session" \0 user \0 key)
// 前 16 位 + "-" + seed（有）或随机 UUID（无）。同一段对话复用同一 session。
func qoderSessionID(userID, upstreamKey, seed string) string {
	h := sha256.New()
	h.Write([]byte("qoder-session"))
	h.Write([]byte{0})
	h.Write([]byte(userID))
	h.Write([]byte{0})
	h.Write([]byte(upstreamKey))
	base := hex.EncodeToString(h.Sum(nil))
	if len(base) > 16 {
		base = base[:16]
	}
	if seed = strings.TrimSpace(seed); seed != "" {
		return base + "-" + seed
	}
	return base + "-" + qoderRandomUUID()
}

// qoderRecordID 请求指纹（protocol.rs:500-528）：sha256("qoder-record" \0 key
// \0 role \0 content … \0 mt=<max_tokens>) 前 16 位。同一段消息 + 同一模型 +
// 同一输出上限 → 同一记录 id（上游据此识别"同一次对话的延续"）。
func qoderRecordID(upstreamKey string, messages []any, tools []any, maxTokens int64) string {
	h := sha256.New()
	h.Write([]byte("qoder-record"))
	h.Write([]byte{0})
	h.Write([]byte(upstreamKey))
	for _, m := range messages {
		msg, isMap := m.(map[string]any)
		if !isMap {
			continue
		}
		role, _ := msg["role"].(string)
		h.Write([]byte{0})
		h.Write([]byte(role))
		if content, ok := msg["content"]; ok && content != nil {
			h.Write([]byte{0})
			switch c := content.(type) {
			case string:
				h.Write([]byte(c))
			default:
				if raw, ok := qoderDeterministicJSON(c); ok {
					h.Write(raw)
				}
			}
		}
	}
	if len(tools) > 0 {
		h.Write([]byte{0})
		if raw, ok := qoderDeterministicJSON(tools); ok {
			h.Write(raw)
		}
	}
	h.Write([]byte{0})
	h.Write([]byte(fmt.Sprintf("mt=%d", maxTokens)))
	sum := hex.EncodeToString(h.Sum(nil))
	if len(sum) > 16 {
		sum = sum[:16]
	}
	return sum
}

// qoderKeyFingerprint machineId 缺省时的确定性派生（文档 §3.3 默认决策）：
// HMAC-SHA256(secret, "qoder-machine") 的 UUID v4 形态。同账号跨重启稳定，
// 不回写 config。
func qoderKeyFingerprint(secret string) string {
	sum := sha256.Sum256([]byte("qoder-machine:" + secret))
	b := append([]byte{}, sum[:16]...)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// 校验身份标识（credentials.rs:168-173）：≤256、不含 `|`、无控制字符。
func qoderValidateIdentity(value string) error {
	if len(value) > 256 || strings.Contains(value, "|") || strings.ContainsFunc(value, func(r rune) bool {
		return r < 0x20 || r == 0x7f
	}) {
		return &HTTPError{Status: 400, Code: "qoder_identity_invalid", Message: "Qoder 用户或设备标识无效"}
	}
	return nil
}
