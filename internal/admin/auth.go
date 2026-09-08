package admin

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"revproxy/internal/config"
)

// Auth 负责管理员登录与会话令牌（HMAC 签名，无状态）
type Auth struct {
	store *config.Store
	mu    sync.RWMutex
}

func NewAuth(store *config.Store) *Auth {
	return &Auth{store: store}
}

func (a *Auth) secret() string {
	return a.store.Get().Admin.Secret
}

func (a *Auth) ttl() time.Duration {
	h := a.store.Get().Admin.SessionTTL
	if h <= 0 {
		h = 12
	}
	return time.Duration(h) * time.Hour
}

// Login 校验账号密码并签发令牌
func (a *Auth) Login(user, pass string) (string, error) {
	adm := a.store.Get().Admin
	if subtle.ConstantTimeCompare([]byte(user), []byte(adm.Username)) != 1 {
		return "", fmt.Errorf("用户名或密码错误")
	}
	if !config.VerifyPassword(pass, adm.PassHash) {
		return "", fmt.Errorf("用户名或密码错误")
	}
	return a.Token(user), nil
}

func (a *Auth) Token(user string) string {
	exp := time.Now().Add(a.ttl()).Unix()
	mac := hmac.New(sha256.New, []byte(a.secret()))
	fmt.Fprintf(mac, "%s:%d", user, exp)
	return fmt.Sprintf("%d.%s.%s", exp, user, hex.EncodeToString(mac.Sum(nil)))
}

// Verify 校验令牌是否有效
func (a *Auth) Verify(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	parts := strings.SplitN(token, ".", 3)
	if len(parts) != 3 {
		return "", false
	}
	exp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", false
	}
	mac := hmac.New(sha256.New, []byte(a.secret()))
	fmt.Fprintf(mac, "%s:%d", parts[1], exp)
	want := hex.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(want), []byte(parts[2])) != 1 {
		return "", false
	}
	return parts[1], true
}

// ChangePassword 修改管理员密码，返回新令牌
func (a *Auth) ChangePassword(oldPass, newPass string) (string, error) {
	adm := a.store.Get().Admin
	if !config.VerifyPassword(oldPass, adm.PassHash) {
		return "", fmt.Errorf("原密码不正确")
	}
	if len(newPass) < 6 {
		return "", fmt.Errorf("新密码至少 6 位")
	}
	var token string
	err := a.store.Update(func(c *config.Config) error {
		c.Admin.Username = adm.Username
		c.Admin.PassHash = config.HashPassword(newPass)
		return nil
	})
	if err != nil {
		return "", err
	}
	token = a.Token(adm.Username)
	return token, nil
}
