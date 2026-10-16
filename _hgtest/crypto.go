package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha512"
	"encoding/base64"
	"errors"
)

// app-source.js constants（64 字节）
var urlConstants = []byte{
	0x4d, 0xd4, 0xc2, 0xe6, 0xb8, 0x31, 0x62, 0x09, 0x0e, 0x52, 0xb3, 0xc7, 0xa6, 0x73, 0x3b, 0xa4,
	0x1c, 0xb2, 0x46, 0x2b, 0x82, 0x9a, 0xb5, 0x8a, 0x19, 0x6b, 0x39, 0xdb, 0x57, 0x17, 0x75, 0x24,
	0xf4, 0x9b, 0xaf, 0x7f, 0x08, 0xe8, 0xd6, 0x8d, 0x26, 0xa7, 0x2e, 0x37, 0xc1, 0xa9, 0x5a, 0x2f,
	0x1f, 0x05, 0xa5, 0x18, 0x92, 0xae, 0xf2, 0x94, 0x97, 0x32, 0xb6, 0x2a, 0x38, 0xaa, 0xdd, 0x58,
}

// decryptSpadeURL: seed 双 sha512 派生 AES-128-CBC 密钥/IV 解密媒体 URL
func decryptSpadeURL(value string, seed []byte) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	if len(raw) < 5 {
		return "", errors.New("spade URL 密文过短")
	}
	if raw[0] != 0xa8 || raw[2] != 0x01 || raw[3] != 0x00 {
		return "", errors.New("spade URL 头格式错误")
	}
	cipherLength := ((len(raw) - 4) / 16) * 16
	if cipherLength == 0 {
		return "", errors.New("spade URL 无完整块")
	}
	h1 := sha512.Sum512(seed)
	h2 := sha512.Sum512(append(append([]byte(nil), h1[:]...), urlConstants...))
	block, err := aes.NewCipher(h2[:16])
	if err != nil {
		return "", err
	}
	plain := make([]byte, cipherLength)
	cipher.NewCBCDecrypter(block, h2[16:32]).CryptBlocks(plain, raw[4:4+cipherLength])
	end := len(plain)
	if end > 0 {
		pad := int(plain[end-1])
		if pad >= 1 && pad <= 16 && pad <= end {
			end -= pad
		}
	}
	for end > 0 && plain[end-1] == 0 {
		end--
	}
	return string(plain[:end]), nil
}
