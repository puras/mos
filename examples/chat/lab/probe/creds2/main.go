// Command creds2 定向尝试从本机 compose 文件里发现的数据库凭据。
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

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var v string
	if err := db.QueryRowContext(ctx, "select version()").Scan(&v); err != nil {
		return false
	}
	fmt.Printf("  [OK] %s\n       -> %s\n", dsn, v)
	return true
}

func main() {
	pg := []string{
		"postgres://llmproxy:dbpassword9090@127.0.0.1:5432/litellm?sslmode=disable",
		"postgres://root:123456@127.0.0.1:5432/new-api?sslmode=disable",
		"postgres://root:123456@127.0.0.1:5432/postgres?sslmode=disable",
		"postgres://postgres:123456@127.0.0.1:5432/postgres?sslmode=disable",
	}
	my := []string{
		"root:OneAPI@justsong@tcp(127.0.0.1:3306)/",
		"oneapi:123456@tcp(127.0.0.1:3306)/one-api",
		"root:123456@tcp(127.0.0.1:3306)/",
	}

	fmt.Println("== postgres ==")
	for _, d := range pg {
		try("pgx", d)
	}
	fmt.Println("\n== mysql ==")
	for _, d := range my {
		try("mysql", d)
	}
}
