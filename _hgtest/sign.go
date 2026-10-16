package main

import (
	"sort"
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/bits"
	"math/rand"
	"net/http"
	"time"
	"strings"
)

// ---- 位运算辅助 ----

func rotl8(x int) int { return ((x << 4) | (x >> 4)) & 255 }

func reverseBits(value int) int {
	r := 0
	for i := 0; i < 8; i++ {
		r = (r << 1) | ((value >> i) & 1)
	}
	return r
}

func le32(v int32) []byte {
	out := make([]byte, 4)
	binary.LittleEndian.PutUint32(out, uint32(v))
	return out
}

func be32(v int32) []byte {
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, uint32(v))
	return out
}

func readU64Le(b []byte, off int) uint64 {
	return binary.LittleEndian.Uint64(b[off : off+8])
}

func putU64Le(b []byte, off int, v uint64) {
	binary.LittleEndian.PutUint64(b[off:off+8], v)
}

func concat(parts ...[]byte) []byte {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func b64(data []byte) string { return base64.StdEncoding.EncodeToString(data) }

// ---- x-gorgon ----

// rc4Gorgon: RC4 变体，KSA 只赋值不交换，PRGA 异或 s[(y+y)&255]
func rc4Gorgon(data []byte, key []int) []byte {
	s := make([]int, 256)
	for i := 0; i < 256; i++ {
		s[i] = i
	}
	j := 0
	for i := 0; i < 256; i++ {
		j = (j + s[i] + key[i%len(key)]) & 255
		s[i] = s[j]
	}
	out := make([]byte, len(data))
	i := 0
	j = 0
	for k := 0; k < len(data); k++ {
		i += 1
		x := s[i]
		j += x
		y := s[j&255]
		s[i] = y
		out[k] = byte(int(data[k]) ^ s[(y+y)&255])
	}
	return out
}

func xGorgon(query string, body []byte, timestamp int64, random int) string {
	q4 := md5.Sum([]byte(query))
	var b4 []byte
	if len(body) > 0 {
		sum := md5.Sum(body)
		b4 = sum[:4]
	} else {
		b4 = make([]byte, 4)
	}
	sdk := le32(67503104)
	input := concat(q4[:4], b4, make([]byte, 4), sdk, be32(int32(timestamp)))
	key := []int{0x4a, 0x40, 0x16, (random >> 8) & 255, 0x47, 0x6c, 0x01, random & 255}
	out := rc4Gorgon(input, key)
	for i := 0; i < len(out); i++ {
		value := rotl8(int(out[i]))
		next := int(out[0])
		if i+1 < len(out) {
			next = int(out[i+1])
		}
		out[i] = byte(^(reverseBits((next^value)&255) ^ 20) & 255)
	}
	prefix, _ := hex.DecodeString("8404")
	tail := []byte{byte(random & 255), byte((random >> 8) & 255), 0x40, 0x01}
	return hex.EncodeToString(concat(prefix, tail, out))
}

// ---- x-helios ----

func helios(khronos int64, random int32) string {
	digest := md5.Sum(concat(le32(random), []byte("8662")))
	hexStr := hex.EncodeToString(digest[:])
	ascii := []byte(hexStr)
	var words [4]uint64
	for i := 0; i < 4; i++ {
		words[i] = readU64Le(ascii, i*8)
	}
	// JS: words.splice(0,2)，队列恒长 2
	queue := []uint64{words[2], words[3]}
	b0 := words[0]
	b8 := words[1]
	table := make([]uint64, 35)
	table[0] = words[0]
	for i := 0; i < 34; i++ {
		x8 := bits.RotateLeft64(b8, -8) + b0
		x8 = x8 ^ uint64(i)
		queue = append(queue, x8)
		x8 = x8 ^ bits.RotateLeft64(b0, -61)
		table[i+1] = x8
		b0 = x8
		b8 = queue[0]
		queue = queue[1:]
	}
	text := fmt.Sprintf("%d-1588093228-8662", khronos)
	tb := []byte(text)
	padLen := ((len(tb) + 1 + 15) / 16) * 16
	pad := make([]byte, padLen)
	fill := byte(16 - (len(tb) % 16))
	for i := range pad {
		pad[i] = fill
	}
	copy(pad, tb)

	var output [][]byte
	for at := 0; at < len(pad); at += 16 {
		a := readU64Le(pad, at)
		b := readU64Le(pad, at+8)
		for i := 0; i < 34; i++ {
			b = table[i] ^ (a + bits.RotateLeft64(b, -8))
			a = b ^ bits.RotateLeft64(a, -61)
		}
		block := make([]byte, 16)
		putU64Le(block, 0, a)
		putU64Le(block, 8, b)
		output = append(output, block)
	}
	all := [][]byte{le32(random)}
	all = append(all, output...)
	return b64(concat(all...))
}

// ---- 签名入口 ----

type hgSign struct {
	gorgon string
	khronos string
	ladon  string
	argus  string
	helios string
}

func signHeaders(query string, body []byte, khronos int64) hgSign {
	random := rand.Intn(65536)
	return hgSign{
		gorgon:  xGorgon(query, body, khronos, random),
		khronos: fmt.Sprintf("%d", khronos),
		ladon:   b64(be32(int32(khronos))),
		argus:   b64(le32(int32(khronos))),
		helios:  helios(khronos, int32(rand.Int31())),
	}
}

// ---- query 编码：python 风格，仅保留 A-Za-z0-9-_.~ ----

func pyEncode(raw string) string {
	var sb strings.Builder
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			sb.WriteByte(c)
		} else {
			sb.WriteString(fmt.Sprintf("%%%02X", c))
		}
	}
	return sb.String()
}

func encodeQuery(pairs [][2]string) string {
	var sb strings.Builder
	for i, kv := range pairs {
		if i > 0 {
			sb.WriteByte('&')
		}
		sb.WriteString(pyEncode(kv[0]))
		sb.WriteByte('=')
		sb.WriteString(pyEncode(kv[1]))
	}
	return sb.String()
}

func queryOf(url string) string {
	if i := strings.IndexByte(url, '?'); i >= 0 {
		return url[i+1:]
	}
	return ""
}

// ---- 旧版签名（video_detail 用） ----

func signLegacy(request *http.Request, body []byte, now time.Time) {
	timestamp := uint32(now.Unix())
	queryHash := md5.Sum([]byte(request.URL.RawQuery))
	var payload [20]byte
	copy(payload[:4], queryHash[:4])
	if body != nil {
		bodyHash := md5.Sum(body)
		copy(payload[4:8], bodyHash[:4])
		request.Header.Set("X-SS-STUB", strings.ToUpper(fmt.Sprintf("%x", bodyHash)))
	}
	copy(payload[12:16], []byte{0, 6, 11, 28})
	binary.BigEndian.PutUint32(payload[16:], timestamp)
	key := [...]byte{0x44, 0xb9, 0xb9, 0xd9, 0xa4, 0xae, 0xf9, 0xfc, 0xa4, 0x93, 0xaa, 0x75, 0x7c, 0xa3, 0xc2, 0xc4, 0xa4, 0x96, 0x93, 0x8f}
	for index := range payload {
		payload[index] ^= key[index]
	}
	for index := range payload {
		mixed := bits.RotateLeft8(payload[index], 4) ^ payload[(index+1)%len(payload)]
		payload[index] = bits.Reverse8(mixed) ^ 0xff ^ byte(len(payload))
	}
	signature := append([]byte{0x84, 0x04, 0x40, 0x1c, 0, 0}, payload[:]...)
	request.Header.Set("X-Khronos", fmt.Sprint(timestamp))
	request.Header.Set("X-Gorgon", hex.EncodeToString(signature))
}

// ---- 小工具 ----

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	if len(out) > 12 {
		out = out[:12]
	}
	return out
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
