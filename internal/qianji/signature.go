package qianji

import (
	"crypto/md5"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PasswordMD5 is the lowercase credential digest required by the wire protocol.
func PasswordMD5(password string) string {
	sum := md5.Sum([]byte(password))
	return hex.EncodeToString(sum[:])
}

func signature(path string, epochMS int64) (string, string) {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	ctrl, act := parts[0], parts[1]
	shifted := strconv.FormatInt(epochMS+1207+9081127, 10)
	reqid := PasswordMD5("com.mutangtech.qianji" + shifted + ctrl + act + "free20170908&x_*1127")
	inner := PasswordMD5(reqid + "michaeljackson")
	return reqid, PasswordMD5(reqid + "1172020" + ctrl + inner + act)
}

func monotonicClock() func(string) (int64, error) {
	var mu sync.Mutex
	last := make(map[string]int64)
	return func(route string) (int64, error) {
		mu.Lock()
		defer mu.Unlock()
		now := max(time.Now().UnixMilli(), last[route]+1)
		last[route] = now
		return now, nil
	}
}
