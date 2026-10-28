// Command creds 批量尝试本机 PG / MySQL 的常见凭据。
// 只做 select version() 这类只读探测。
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
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
	pgUsers := []string{"postgres", "root", "admin", "mos", "pguser", "app"}
	pgPasses := []string{"", "postgres", "password", "123456", "admin", "root", "mos", "pg", "postgres123", "changeme", "secret"}
	pgDbs := []string{"postgres", "mos", "app"}

	fmt.Println("== postgres ==")
	hit := false
	for _, u := range pgUsers {
		for _, p := range pgPasses {
			for _, d := range pgDbs {
				dsn := fmt.Sprintf("postgres://%s:%s@127.0.0.1:5432/%s?sslmode=disable", u, p, d)
				if try("pgx", dsn) {
					hit = true
				}
			}
		}
	}

	myUsers := []string{"root", "mysql", "admin", "mos", "app"}
	myPasses := []string{"", "root", "password", "123456", "admin", "mysql", "mos", "secret"}
	fmt.Println("\n== mysql ==")
	for _, u := range myUsers {
		for _, p := range myPasses {
			dsn := fmt.Sprintf("%s:%s@tcp(127.0.0.1:3306)/", u, p)
			if try("mysql", dsn) {
				hit = true
			}
		}
	}

	if !hit {
		fmt.Println("\n没有可用凭据。")
		os.Exit(1)
	}
}
