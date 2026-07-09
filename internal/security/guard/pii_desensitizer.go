package guard

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"sync"
)

// PIIDesensitizer 格式保留假数据脱敏器。
// 同一次会话内，同一个真实值始终映射到同一个假值。
type PIIDesensitizer struct {
	mu      sync.RWMutex
	mapping map[string]string // originalValue -> fakeValue
}

func NewPIIDesensitizer() *PIIDesensitizer {
	return &PIIDesensitizer{
		mapping: make(map[string]string),
	}
}

// Desensitize 将原始值转换为同格式假数据。
func (d *PIIDesensitizer) Desensitize(piiType, original string) string {
	d.mu.RLock()
	if fake, ok := d.mapping[original]; ok {
		d.mu.RUnlock()
		return fake
	}
	d.mu.RUnlock()

	var fake string
	switch piiType {
	case "email":
		fake = generateFakeEmail()
	case "phone_cn":
		fake = generateFakePhoneCN(original)
	case "phone_intl":
		fake = generateFakePhoneIntl(original)
	case "id_card_cn":
		fake = generateFakeIDCard()
	case "credit_card":
		fake = generateFakeCreditCard()
	case "ip":
		fake = generateFakeIP(original)
	default:
		// 兜底：如果是不识别的类型或 presidio type，生成长度相近的不可读字符或简单替换
		fake = fmt.Sprintf("REDACTED-%s", randomHex(4))
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	// Double check
	if existing, ok := d.mapping[original]; ok {
		return existing
	}
	d.mapping[original] = fake
	return fake
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func generateFakeEmail() string {
	return fmt.Sprintf("test-%s@example.com", randomHex(4))
}

func generateFakePhoneCN(orig string) string {
	// 保留前3位，如果带 +86 则保留前面部分
	orig = strings.TrimSpace(orig)
	prefixLen := 3
	if strings.HasPrefix(orig, "+86") {
		prefixLen = 6 // +86139
	} else if strings.HasPrefix(orig, "0") {
		prefixLen = 4 // 0139
	}

	if len(orig) <= prefixLen {
		return orig
	}

	prefix := orig[:prefixLen]
	// 中国手机号是 11 位（不含前缀）。这里假设后8位随机生成
	var suffix string
	for i := 0; i < len(orig)-prefixLen; i++ {
		n, _ := rand.Int(rand.Reader, big.NewInt(10))
		suffix += n.String()
	}
	return prefix + suffix
}

func generateFakePhoneIntl(orig string) string {
	orig = strings.TrimSpace(orig)
	if !strings.HasPrefix(orig, "+") {
		return orig
	}

	// 保留前3个字符(如 +1, +44, +33)
	prefixLen := 3
	if len(orig) <= prefixLen {
		return orig
	}
	prefix := orig[:prefixLen]

	var suffix string
	for i := 0; i < len(orig)-prefixLen; i++ {
		n, _ := rand.Int(rand.Reader, big.NewInt(10))
		suffix += n.String()
	}
	return prefix + suffix
}

func generateFakeIDCard() string {
	// 避免使用真实存在的前缀。使用 999999 作为不可能存在的地区码。
	region := "999999"
	// 出生日期: 19900101
	birthday := "19900101"

	// 顺序码
	seqB := make([]byte, 1)
	_, _ = rand.Read(seqB)
	seqInt := int(seqB[0]) % 1000
	seq := fmt.Sprintf("%03d", seqInt)

	base := region + birthday + seq

	// 计算校验位
	weight := []int{7, 9, 10, 5, 8, 4, 2, 1, 6, 3, 7, 9, 10, 5, 8, 4, 2}
	checkCode := []byte{'1', '0', 'X', '9', '8', '7', '6', '5', '4', '3', '2'}

	sum := 0
	for i := 0; i < 17; i++ {
		num, _ := strconv.Atoi(string(base[i]))
		sum += num * weight[i]
	}

	mod := sum % 11
	return base + string(checkCode[mod])
}

func generateFakeCreditCard() string {
	// 411111111111111 - 15 chars base for test Visa
	base := "411111111111111"

	sum := 0
	for i := 0; i < len(base); i++ {
		digit := int(base[i] - '0')
		if i%2 == 0 {
			digit *= 2
			if digit > 9 {
				digit -= 9
			}
		}
		sum += digit
	}

	check := (10 - (sum % 10)) % 10
	return base + strconv.Itoa(check)
}

func generateFakeIP(orig string) string {
	// 替换为 RFC 5737 预留网段 198.51.100.X
	// 对于 IPv6 可以考虑 RFC 3849 预留 2001:DB8::/32
	if strings.Contains(orig, ":") {
		// IPv6
		return "2001:db8::1234:5678"
	}
	// IPv4
	n, _ := rand.Int(rand.Reader, big.NewInt(254))
	return fmt.Sprintf("198.51.100.%d", n.Int64()+1)
}

func (d *PIIDesensitizer) Clear() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.mapping = make(map[string]string)
}
