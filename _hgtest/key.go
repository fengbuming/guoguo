package main

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os/exec"
	"math/bits"
	"strconv"
	"strings"
)

func decodeHongguoBase64(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	decoded, err := base64.StdEncoding.Strict().DecodeString(value)
	if err == nil {
		return decoded, nil
	}
	return base64.RawStdEncoding.Strict().DecodeString(value)
}

// hongguoContentKeyVariant: subtraction=false 用 app 版减法；true 用 web 版加法
func hongguoContentKeyVariant(value string, addition bool) ([]byte, string, error) {
	if len(value) > 1024 {
		return nil, "", fmt.Errorf("key data too long")
	}
	raw, err := decodeHongguoBase64(value)
	if err != nil || len(raw) < 3 {
		return nil, "", fmt.Errorf("key encoding invalid")
	}
	tagLength := int(raw[0]^raw[1]^raw[2]) - 48
	contentLength := len(raw) - tagLength - 1
	if tagLength < 1 || contentLength < 33 || contentLength >= len(raw) {
		return nil, "", fmt.Errorf("key structure invalid")
	}
	seed := raw[len(raw)-tagLength-2] ^ raw[len(raw)-tagLength-1]
	tag := make([]byte, tagLength)
	for index := range tag {
		tag[index] = raw[len(raw)-tagLength+index] ^ seed
	}
	decoded := make([]byte, contentLength)
	previousEven, previousOdd := byte(250), byte(85)
	for index, current := range raw[1 : 1+contentLength] {
		previous := previousEven
		if index%2 == 0 {
			previousEven = current
		} else {
			previous = previousOdd
			previousOdd = current
		}
		if addition {
			decoded[index] = byte(int(previous^current) + 21 + bits.OnesCount(uint(index)))
		} else {
			decoded[index] = byte(int(previous^current) - 21 - bits.OnesCount(uint(index)))
		}
	}
	paddingVal, perr := strconv.ParseInt(string(decoded[:1]), 36, 8)
	if perr != nil || contentLength-int(paddingVal)-1 != 32 {
		return nil, string(tag), fmt.Errorf("key content invalid (padding=%d err=%v)", paddingVal, perr)
	}
	key, err := hex.DecodeString(string(decoded[1:33]))
	if err != nil || len(key) != 16 {
		return nil, string(tag), fmt.Errorf("key is not valid AES-128")
	}
	return key, string(tag), nil
}

// ffmpegProbe: 用派生密钥解密 3 秒，返回可读结论
func ffmpegProbe(url, hexKey string) string {
	out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "info",
		"-decryption_key", hexKey,
		"-headers", "User-Agent: "+videoUA+"\r\n",
		"-t", "3", "-i", url, "-f", "null", "-").CombinedOutput()
	text := string(out)
	if err != nil {
		for _, line := range strings.Split(text, "\n") {
			if strings.Contains(line, "Error") || strings.Contains(line, "Invalid") ||
				strings.Contains(line, "error") {
				return "FAIL: " + strings.TrimSpace(line)
			}
		}
		return "FAIL: " + err.Error()
	}
	frames := "?"
	codec := "?"
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "frame=") {
			frames = strings.TrimSpace(strings.SplitN(trimmed, " ", 2)[0])
		}
		if strings.Contains(trimmed, "Stream #") && strings.Contains(trimmed, "Video:") {
			codec = truncate(trimmed, 90)
		}
	}
	return fmt.Sprintf("OK frames=%s | %s", frames, codec)
}
