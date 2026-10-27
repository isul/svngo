// Package passwd creates and verifies Apache htpasswd compatible hashes.
package passwd

import (
	"bufio"
	"crypto/md5"
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"io"
	"strings"

	"svngo/internal/validate"

	"golang.org/x/crypto/bcrypt"
)

// Hash returns an Apache htpasswd compatible bcrypt hash ($2y$).
func Hash(pw string, cost int) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), cost)
	if err != nil {
		return "", err
	}
	// Go emits $2a$; Apache's htpasswd writes $2y$. Both are accepted by APR
	// and by Go, $2y$ keeps files identical to what htpasswd -B produces.
	return "$2y$" + string(h[4:]), nil
}

// Verify checks a password against a bcrypt or APR1-MD5 hash.
func Verify(hash, pw string) bool {
	switch {
	case strings.HasPrefix(hash, "$2"):
		return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
	case strings.HasPrefix(hash, "$apr1$"):
		parts := strings.Split(hash, "$")
		if len(parts) != 4 {
			return false
		}
		calc := apr1(pw, parts[2])
		return subtle.ConstantTimeCompare([]byte(calc), []byte(hash)) == 1
	}
	return false
}

func HashType(hash string) string {
	switch {
	case strings.HasPrefix(hash, "$2"):
		return "bcrypt"
	case strings.HasPrefix(hash, "$apr1$"):
		return "apr1"
	}
	return "unknown"
}

// ValidStoredHash reports whether hash is a format SvnGo writes to htpasswd.
func ValidStoredHash(hash string) bool {
	if strings.ContainsAny(hash, ":\r\n \t") {
		return false
	}
	switch {
	case len(hash) == 60 && (strings.HasPrefix(hash, "$2y$") || strings.HasPrefix(hash, "$2a$") || strings.HasPrefix(hash, "$2b$")):
		return true
	case strings.HasPrefix(hash, "$apr1$"):
		parts := strings.Split(hash, "$")
		return len(parts) == 4 && len(parts[2]) >= 1 && len(parts[2]) <= 8 && len(parts[3]) == 22
	}
	return false
}

const itoa64 = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// apr1 implements Apache's APR1-MD5 crypt (used only to verify/migrate legacy hashes).
func apr1(password, salt string) string {
	const magic = "$apr1$"
	if len(salt) > 8 {
		salt = salt[:8]
	}
	pw := []byte(password)
	h := md5.New()
	h.Write(pw)
	h.Write([]byte(magic))
	h.Write([]byte(salt))

	alt := md5.New()
	alt.Write(pw)
	alt.Write([]byte(salt))
	alt.Write(pw)
	altSum := alt.Sum(nil)
	for i := len(pw); i > 0; i -= 16 {
		h.Write(altSum[:min(16, i)])
	}
	for i := len(pw); i > 0; i >>= 1 {
		if i&1 != 0 {
			h.Write([]byte{0})
		} else {
			h.Write(pw[:1])
		}
	}
	final := h.Sum(nil)
	for i := 0; i < 1000; i++ {
		h2 := md5.New()
		if i&1 != 0 {
			h2.Write(pw)
		} else {
			h2.Write(final)
		}
		if i%3 != 0 {
			h2.Write([]byte(salt))
		}
		if i%7 != 0 {
			h2.Write(pw)
		}
		if i&1 != 0 {
			h2.Write(final)
		} else {
			h2.Write(pw)
		}
		final = h2.Sum(nil)
	}
	var out strings.Builder
	enc := func(v uint32, n int) {
		for ; n > 0; n-- {
			out.WriteByte(itoa64[v&0x3f])
			v >>= 6
		}
	}
	f := func(i int) uint32 { return uint32(final[i]) }
	enc(f(0)<<16|f(6)<<8|f(12), 4)
	enc(f(1)<<16|f(7)<<8|f(13), 4)
	enc(f(2)<<16|f(8)<<8|f(14), 4)
	enc(f(3)<<16|f(9)<<8|f(15), 4)
	enc(f(4)<<16|f(10)<<8|f(5), 4)
	enc(f(11), 2)
	return magic + salt + "$" + out.String()
}

// RandomString returns n characters drawn from alphabet using crypto/rand.
func RandomString(n int, alphabet string) string {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// Generate returns a random 16 character password without ambiguous characters.
func Generate() string {
	return RandomString(16, "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789")
}

type Entry struct {
	User, Hash string
}

// ParseHtpasswd parses an htpasswd file for import. Unsupported hash formats
// are returned in rejected with a reason.
func ParseHtpasswd(r io.Reader) (ok []Entry, rejected []string, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 64*1024)
	seen := map[string]bool{}
	line := 0
	for sc.Scan() {
		line++
		s := strings.TrimSpace(sc.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		user, hash, found := strings.Cut(s, ":")
		if !found {
			rejected = append(rejected, fmt.Sprintf("%d행: 형식 오류", line))
			continue
		}
		if err := validate.Username(user); err != nil {
			rejected = append(rejected, fmt.Sprintf("%d행: 사용자 이름 %q 사용 불가", line, validate.Clip(user, 40)))
			continue
		}
		if !ValidStoredHash(hash) {
			rejected = append(rejected, fmt.Sprintf("%s: 지원하지 않는 해시 형식 (bcrypt, APR1-MD5 만 지원)", user))
			continue
		}
		if seen[user] {
			rejected = append(rejected, fmt.Sprintf("%s: 중복 항목", user))
			continue
		}
		seen[user] = true
		ok = append(ok, Entry{user, hash})
	}
	return ok, rejected, sc.Err()
}
