// Package main — 一次性管理员密码重置工具。
//
// 用法：
//
//	# 列出所有 admin 账号
//	go run ./scripts/reset-admin-password -db /DATA/AppData/llms_proxy/data/llms-proxy.db -list
//
//	# 重置指定账号密码
//	go run ./scripts/reset-admin-password -db /DATA/AppData/llms_proxy/data/llms-proxy.db -user admin -password suntao341
//
// 注意：重置时必须停止 llms-proxy 容器，否则 bbolt 会被锁住。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/ycgame/llms-proxy/internal/admin"
	"github.com/ycgame/llms-proxy/internal/config"
)

const bucketAdminUsers = "admin_users"

func main() {
	var (
		dbPath   string
		username string
		password string
		list     bool
	)
	flag.StringVar(&dbPath, "db", "", "bbolt database path (required)")
	flag.StringVar(&username, "user", "admin", "username to reset")
	flag.StringVar(&password, "password", "", "new password (required unless -list)")
	flag.BoolVar(&list, "list", false, "only list existing users")
	flag.Parse()

	if strings.TrimSpace(dbPath) == "" {
		fmt.Fprintln(os.Stderr, "error: -db is required")
		flag.Usage()
		os.Exit(2)
	}

	db, err := bolt.Open(dbPath, 0o600, &bolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		log.Fatalf("open bbolt: %v (tip: stop the llms-proxy container first)", err)
	}
	defer db.Close()

	if list {
		if err := listUsers(db); err != nil {
			log.Fatalf("list users: %v", err)
		}
		return
	}

	if strings.TrimSpace(password) == "" {
		log.Fatalf("error: -password is required when not listing")
	}

	if err := resetPassword(db, username, password); err != nil {
		log.Fatalf("reset password: %v", err)
	}
	fmt.Printf("密码已重置：用户 %q\n", username)
}

func listUsers(db *bolt.DB) error {
	return db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketAdminUsers))
		if b == nil {
			fmt.Println("(admin_users bucket 不存在 - DB 可能为新初始化)")
			return nil
		}
		count := 0
		_ = b.ForEach(func(k, v []byte) error {
			var u config.AdminUser
			_ = json.Unmarshal(v, &u)
			disabled := ""
			if u.Disabled {
				disabled = " [DISABLED]"
			}
			fmt.Printf("- key=%s  username=%s  role=%s  hash_prefix=%s%s\n",
				string(k), u.Username, u.Role, firstN(u.PasswordHash, 20), disabled)
			count++
			return nil
		})
		fmt.Printf("共 %d 个 admin 用户\n", count)
		return nil
	})
}

func resetPassword(db *bolt.DB, username, password string) error {
	key := strings.ToLower(strings.TrimSpace(username))
	newHash, err := admin.HashPasswordWithRandomSalt(password)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	return db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(bucketAdminUsers))
		if err != nil {
			return err
		}
		var u config.AdminUser
		if data := b.Get([]byte(key)); data != nil {
			if err := json.Unmarshal(data, &u); err != nil {
				return fmt.Errorf("decode existing user: %w", err)
			}
		} else {
			fmt.Printf("(用户 %q 不存在，将新建)\n", username)
			u.Username = username
		}
		if strings.TrimSpace(u.Username) == "" {
			u.Username = username
		}
		if strings.TrimSpace(u.Role) == "" {
			u.Role = "admin"
		}
		u.Disabled = false
		u.PasswordHash = newHash
		data, err := json.Marshal(u)
		if err != nil {
			return fmt.Errorf("encode user: %w", err)
		}
		return b.Put([]byte(key), data)
	})
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
