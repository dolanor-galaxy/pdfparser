package pdf

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rc4"
	"encoding/binary"
)

var padding_string []byte = []byte("\x28\xBF\x4E\x5E\x4E\x75\x8A\x41\x64\x00\x4E\x56\xFF\xFA\x01\x08\x2E\x2E\x00\xB6\xD0\x68\x3E\x80\x2F\x0C\xA9\xFE\x64\x53\x69\x7A")
var noFilter = &CryptFilterNone{}

type CryptFilter interface {
	Init(int, int) CryptFilter
	Decrypt([]byte) []byte
}

// No encryption
type CryptFilterNone struct {}

func (c *CryptFilterNone) Init(n int, g int) CryptFilter {
	return c
}

func (c *CryptFilterNone) Decrypt(data []byte) []byte {
	return data
}

// AES
type CryptFilterAES struct {
	encryption_key []byte
}

func (c *CryptFilterAES) Init(n int, g int) CryptFilter {
	// allocate space for salt and copy encryption key into it
	salt := make([]byte, len(c.encryption_key), len(c.encryption_key) + 9)
	copy(salt, c.encryption_key)

	// get n as byte little endian byte array, add first 3 bytes to salt
	nb := make([]byte, 4)
	binary.LittleEndian.PutUint32(nb, uint32(n))
	salt = append(salt, nb[:3]...)

	// get g as byte little endian byte array, add first 2 bytes to salt
	gb := make([]byte, 4)
	binary.LittleEndian.PutUint32(gb, uint32(g))
	salt = append(salt, gb[:2]...)

	// add sAlT to key
	salt = append(salt, []byte("sAlT")...)

	// hash the salt to produce the key
	hash := md5.New()
	hash.Write(salt)
	key := hash.Sum(nil)

	// trucate key to length + 5 max 16
	l := len(c.encryption_key) + 5
	if l > 16 {
		l = 16
	}
	key = key[:l]

	// return new crypt filter with salted key
	return &CryptFilterAES{key}
}

func (c *CryptFilterAES) Decrypt(data []byte) []byte {
	block, _ := aes.NewCipher(c.encryption_key)

	// no data to decrypt, first block is initialization vector
	if len(data) <= aes.BlockSize {
		return []byte{}
	}

	// set iv to first block and decrypt remaining blocks with cbc decryptor
	iv := data[:aes.BlockSize]
	data = data[aes.BlockSize:]
	cbc := cipher.NewCBCDecrypter(block, iv)
	cbc.CryptBlocks(data, data)
	return data
}

// RC4
type CryptFilterRC4 struct {
	encryption_key []byte
}

func (c *CryptFilterRC4) Init(n int, g int) CryptFilter {
	// allocate space for salt and copy encryption key into it
	salt := make([]byte, len(c.encryption_key), len(c.encryption_key) + 5)
	copy(salt, c.encryption_key)

	// get n as byte little endian byte array, add first 3 bytes to salt
	nb := make([]byte, 4)
	binary.LittleEndian.PutUint32(nb, uint32(n))
	salt = append(salt, nb[:3]...)

	// get g as byte little endian byte array, add first 2 bytes to salt
	gb := make([]byte, 4)
	binary.LittleEndian.PutUint32(gb, uint32(g))
	salt = append(salt, gb[:2]...)

	// hash the salt to produce the key
	hash := md5.New()
	hash.Write(salt)
	key := hash.Sum(nil)

	// trucate key to length + 5 max 16
	l := len(c.encryption_key) + 5
	if l > 16 {
		l = 16
	}
	key = key[:l]

	// return new crypt filter with salted key
	return &CryptFilterRC4{key}
}

func (c *CryptFilterRC4) Decrypt(data []byte) []byte {
	cipher, _ := rc4.NewCipher(c.encryption_key)
	cipher.XORKeyStream(data, data)
	return data
}

type SecurityHandler struct {
	v int
	length int
	r int
	o []byte
	u []byte
	p []byte
	encrypt_meta_data bool
	id []byte
	stream_filter CryptFilter
	string_filter CryptFilter
	file_filter CryptFilter
	crypt_filters map[string]CryptFilter
	encryption_key []byte
}

func NewSecurityHandler(password []byte, trailer Dictionary) (*SecurityHandler, error) {
	sh := &SecurityHandler{}

	// get the encrypt dictionary
	encrypt, err := trailer.GetDictionary("Encrypt")
	if err != nil {
		return sh, NewError("Encrypt dictionary not found")
	}

	// get filter
	filter, err := encrypt.GetName("Filter")
	if err != nil {
		return sh, NewError("Encrypt dictionary missing required Filter field")
	}

	// filter is not supported
	if filter != "Standard" {
		return sh, NewError("Unsupported encryption filter")
	}

	// get V
	sh.v, _ = encrypt.GetInt("V")
	if sh.v != 1 && sh.v != 2 && sh.v != 4 {
		return sh, NewError("Unsupported encryption version")
	}

	// get R
	sh.r, err = encrypt.GetInt("R")
	if err != nil {
		return sh, NewError("Encrypt dictionary missing required R field")
	}
	if sh.r < 2 || sh.r > 4 {
		return sh, NewError("Unsupported encryption revision")
	}

	// get Length
	if sh.v == 1 {
		sh.length = 40
	} else {
		sh.length, err = encrypt.GetInt("Length")
		if err != nil {
			sh.length = 40
		}
	}
	sh.length = sh.length/8
	if sh.length < 5 {
		sh.length = 5
	} else if sh.length > 16 {
		sh.length = 16
	}

	// get O
	sh.o, err = encrypt.GetBytes("O")
	if err != nil {
		return sh, NewError("Encrypt dictionary missing required O field")
	}

	// get U
	sh.u, err = encrypt.GetBytes("U")
	if err != nil {
		return sh, NewError("Encrypt dictionary missing required U field")
	}

	// get P
	p, err := encrypt.GetInt("P")
	if err != nil {
		return sh, NewError("Encrypt dictionary missing required P field")
	}
	sh.p = make([]byte, 4)
	binary.LittleEndian.PutUint32(sh.p, uint32(p))

	// get EncryptMetadata
	sh.encrypt_meta_data, err = encrypt.GetBool("EncryptMetadata")
	if err != nil {
		sh.encrypt_meta_data = true
	}

	// get ID[0] from trailer
	ids, err := trailer.GetArray("ID")
	if err != nil {
		return sh, NewError("Trailer dictionary missing required ID field")
	}
	sh.id, err = ids.GetBytes(0)
	if err != nil {
		return sh, NewError("Trailer dictionary missing required ID[0] field")
	}

	// compute encryption key from password
	sh.encryption_key = sh.computeEncryptionKey(password, sh.length)

	// verify key
	if sh.r == 2 { // if revision 2 use algorithm 4
		u := make([]byte, 32)
		cipher, _ := rc4.NewCipher(sh.encryption_key)
		cipher.XORKeyStream(u, padding_string)
		if string(u) != string(sh.u) {
			return sh, ErrorPassword
		}
	} else if sh.r >= 3 { // for revision 3+ use algorithm 5
		// step b, c
		hash := md5.New()
		hash.Write(padding_string)
		hash.Write(sh.id)
		u := hash.Sum(nil)

		// step d, e
		temp_key := make([]byte, len(sh.encryption_key))
		for i := 0; i < 20; i++ {
			for j := range sh.encryption_key {
				temp_key[j] = sh.encryption_key[j] ^ byte(i)
			}
			cipher, _ := rc4.NewCipher(temp_key)
			cipher.XORKeyStream(u, u)
		}

		// compare to first 16 bytes of U entry
		if string(u) != string(sh.u[:16]) {
			return sh, ErrorPassword
		}
	}

	// set default crypt filters
	sh.stream_filter = &CryptFilterRC4{sh.encryption_key}
	sh.string_filter = sh.stream_filter
	sh.file_filter = sh.stream_filter
	sh.crypt_filters = map[string]CryptFilter{}
	sh.crypt_filters["Identity"] = noFilter

	// load additional crypt filters
	if sh.r == 4 {
		cf, _ := encrypt.GetDictionary("CF")
		for k, entry := range cf {
			if cfd, isDictionary := entry.(Dictionary); isDictionary {
				if method, err := cfd.GetName("CFM"); err == nil {
					// get optional length
					length, err := cfd.GetInt("Length")
					if err != nil {
						length = sh.length
					}

					// create filter entry
					if method == "None" {
						sh.crypt_filters[k] = noFilter
					} else if method == "V2" {
						sh.crypt_filters[k] = &CryptFilterRC4{sh.computeEncryptionKey(password, length)}
					} else if method == "AESV2" {
						sh.crypt_filters[k] = &CryptFilterAES{sh.computeEncryptionKey(password, length)}
					}
				}
			}
		}

		// assign default filter overrides
		if name, err := encrypt.GetName("StmF"); err == nil {
			if filter, exists := sh.crypt_filters[name]; exists {
				sh.stream_filter = filter
			}
		}
		if name, err := encrypt.GetName("StrF"); err == nil {
			if filter, exists := sh.crypt_filters[name]; exists {
				sh.string_filter = filter
			}
		}
		if name, err := encrypt.GetName("EEF"); err == nil {
			if filter, exists := sh.crypt_filters[name]; exists {
				sh.file_filter = filter
			}
		}
	}

	// return the sercuirty handler
	return sh, nil
}

// Algorithm 2: Computing an encryption key
func (sh *SecurityHandler) computeEncryptionKey(password []byte, key_length int) []byte {
	// step a) pad or truncate password to exactly 32 bytes
	if len(password) < 32 {
		password = append(password, padding_string[:32 - len(password)]...)
	} else {
		password = password[:32]
	}

	// step b, c, d, e, f, g
	hash := md5.New()
	hash.Write(password)
	hash.Write(sh.o)
	hash.Write(sh.p)
	hash.Write(sh.id)
	if sh.r >= 4 && !sh.encrypt_meta_data {
		hash.Write([]byte("\xff\xff\xff\xff"))
	}
	encryption_key := hash.Sum(nil)[:key_length]

	// step h) for revision 3+, re-hash key 50 times
	if sh.r >= 3 {
		for i := 0; i < 50; i++ {
			hash = md5.New()
			hash.Write(encryption_key)
			encryption_key = hash.Sum(nil)[:key_length]
		}
	}

	return encryption_key
}
