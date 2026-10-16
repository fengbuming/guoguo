package app

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"math/bits"
	"strconv"
	"strings"
	"time"
)

// 红果 App 取址（multi_video_model）使用的新版签名。
//
// 老版 X-Gorgon 只覆盖 video_detail / 目录等接口；取址接口要求六个签名头
// （x-gorgon / x-khronos / x-ladon / x-argus / x-helios / x-medusa），
// 缺失或不被接受时上游一律返回 HTTP 200 + 空 body。
// 这里的 x-gorgon 与老版常量不同（sdk 段为 67503104），不能混用。

const hongguoModelSDKVersion = 67503104

type hongguoModelSignature struct {
	gorgon  string
	khronos string
	ladon   string
	argus   string
	helios  string
}

func hongguoRotl8(value int) int { return ((value << 4) | (value >> 4)) & 255 }

func hongguoReverseBits(value int) int {
	result := 0
	for index := 0; index < 8; index++ {
		result = (result << 1) | ((value >> index) & 1)
	}
	return result
}

func hongguoLe32(value int32) []byte {
	out := make([]byte, 4)
	binary.LittleEndian.PutUint32(out, uint32(value))
	return out
}

func hongguoBe32(value int32) []byte {
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, uint32(value))
	return out
}

func hongguoConcat(parts ...[]byte) []byte {
	size := 0
	for _, part := range parts {
		size += len(part)
	}
	out := make([]byte, 0, size)
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}

func hongguoRandomUint16() int {
	var raw [2]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return int(time.Now().UnixNano()) & 0xffff
	}
	return int(binary.BigEndian.Uint16(raw[:]))
}

func hongguoRandomInt32() int32 {
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return int32(time.Now().UnixNano())
	}
	return int32(binary.BigEndian.Uint32(raw[:]))
}

// hongguoRC4Gorgon: KSA 只赋值不交换，PRGA 异或 s[(y+y)&255] 的 RC4 变体。
func hongguoRC4Gorgon(data []byte, key []int) []byte {
	state := make([]int, 256)
	for index := range state {
		state[index] = index
	}
	j := 0
	for i := 0; i < 256; i++ {
		j = (j + state[i] + key[i%len(key)]) & 255
		state[i] = state[j]
	}
	out := make([]byte, len(data))
	i, k := 0, 0
	for ; k < len(data); k++ {
		i += 1
		x := state[i]
		j += x
		y := state[j&255]
		state[i] = y
		out[k] = byte(int(data[k]) ^ state[(y+y)&255])
	}
	return out
}

func hongguoXGorgon(query string, body []byte, timestamp int64, random int) string {
	queryHash := md5.Sum([]byte(query))
	var bodyPrefix []byte
	if len(body) > 0 {
		bodyHash := md5.Sum(body)
		bodyPrefix = bodyHash[:4]
	} else {
		bodyPrefix = make([]byte, 4)
	}
	input := hongguoConcat(queryHash[:4], bodyPrefix, make([]byte, 4),
		hongguoLe32(hongguoModelSDKVersion), hongguoBe32(int32(timestamp)))
	key := []int{0x4a, 0x40, 0x16, (random >> 8) & 255, 0x47, 0x6c, 0x01, random & 255}
	out := hongguoRC4Gorgon(input, key)
	for index := range out {
		value := hongguoRotl8(int(out[index]))
		next := int(out[0])
		if index+1 < len(out) {
			next = int(out[index+1])
		}
		out[index] = byte(^(hongguoReverseBits((next^value)&255) ^ 20) & 255)
	}
	prefix, _ := hex.DecodeString("8404")
	tail := []byte{byte(random & 255), byte((random >> 8) & 255), 0x40, 0x01}
	return hex.EncodeToString(hongguoConcat(prefix, tail, out))
}

// hongguoHelios: md5(random_le32 + "8662") 派生 35 项表，再对 "<khronos>-1588093228-8662"
// 做 34 轮 Feistel 混淆，输出 base64(random_le32 + 密文块)。
func hongguoHelios(khronos int64, random int32) string {
	digest := md5.Sum(hongguoConcat(hongguoLe32(random), []byte("8662")))
	ascii := []byte(hex.EncodeToString(digest[:]))
	var words [4]uint64
	for index := range words {
		words[index] = binary.LittleEndian.Uint64(ascii[index*8:])
	}
	// JS 侧 words.splice(0,2)：队列恒长 2，b8 取最早入队未消费的元素。
	queue := []uint64{words[2], words[3]}
	b0, b8 := words[0], words[1]
	table := make([]uint64, 35)
	table[0] = words[0]
	for index := 0; index < 34; index++ {
		mixed := bits.RotateLeft64(b8, -8) + b0
		mixed ^= uint64(index)
		queue = append(queue, mixed)
		mixed ^= bits.RotateLeft64(b0, -61)
		table[index+1] = mixed
		b0 = mixed
		b8 = queue[0]
		queue = queue[1:]
	}
	text := []byte(strings.Join([]string{
		strconv.FormatInt(khronos, 10), "1588093228", "8662"}, "-"))
	padding := make([]byte, ((len(text)+1+15)/16)*16)
	fill := byte(16 - (len(text) % 16))
	for index := range padding {
		padding[index] = fill
	}
	copy(padding, text)

	var blocks [][]byte
	for at := 0; at < len(padding); at += 16 {
		a := binary.LittleEndian.Uint64(padding[at:])
		b := binary.LittleEndian.Uint64(padding[at+8:])
		for index := 0; index < 34; index++ {
			b = table[index] ^ (a + bits.RotateLeft64(b, -8))
			a = b ^ bits.RotateLeft64(a, -61)
		}
		block := make([]byte, 16)
		binary.LittleEndian.PutUint64(block, a)
		binary.LittleEndian.PutUint64(block[8:], b)
		blocks = append(blocks, block)
	}
	all := [][]byte{hongguoLe32(random)}
	all = append(all, blocks...)
	return base64.StdEncoding.EncodeToString(hongguoConcat(all...))
}

func signHongguoModel(query string, body []byte, khronos int64) hongguoModelSignature {
	return hongguoModelSignature{
		gorgon:  hongguoXGorgon(query, body, khronos, hongguoRandomUint16()),
		khronos: strconv.FormatInt(khronos, 10),
		ladon:   base64.StdEncoding.EncodeToString(hongguoBe32(int32(khronos))),
		argus:   base64.StdEncoding.EncodeToString(hongguoLe32(int32(khronos))),
		helios:  hongguoHelios(khronos, hongguoRandomInt32()),
	}
}

// encodeHongguoModelQuery: 复刻 python urllib.parse.quote 的兜底行为
// —— 只保留 A-Za-z0-9-_.~，其余（含 !'()* 与空格）一律转成大写 %XX。
// 上游校验签名用的就是这一串，编码不一致会直接被判为签名无效。
func encodeHongguoModelQuery(pairs [][2]string) string {
	var builder strings.Builder
	for index, pair := range pairs {
		if index > 0 {
			builder.WriteByte('&')
		}
		builder.WriteString(hongguoPyEncode(pair[0]))
		builder.WriteByte('=')
		builder.WriteString(hongguoPyEncode(pair[1]))
	}
	return builder.String()
}

func hongguoPyEncode(raw string) string {
	var builder strings.Builder
	for index := 0; index < len(raw); index++ {
		char := raw[index]
		if (char >= 'A' && char <= 'Z') || (char >= 'a' && char <= 'z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_' ||
			char == '.' || char == '~' {
			builder.WriteByte(char)
			continue
		}
		builder.WriteByte('%')
		const digits = "0123456789ABCDEF"
		builder.WriteByte(digits[char>>4])
		builder.WriteByte(digits[char&15])
	}
	return builder.String()
}
