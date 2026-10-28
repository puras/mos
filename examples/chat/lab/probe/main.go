// Command probe 探测本机 5432 / 3306 上跑的是什么数据库，以及能否认证。
// 只读探测，不做任何修改。
package main

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func try(driver, dsn string) {
	db, err := sql.Open(driver, dsn)
	if err != nil {
		fmt.Printf("  [%s] open error: %v\n", dsn, err)
		return
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var v string
	if err := db.QueryRowContext(ctx, "select version()").Scan(&v); err != nil {
		fmt.Printf("  [FAIL] %s\n         -> %v\n", dsn, err)
		return
	}
	fmt.Printf("  [OK]   %s\n         -> %s\n", dsn, v)
}

func main() {
	fmt.Println("== postgres candidates ==")
	for _, dsn := range []string{
		"postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable",
		"postgres://postgres:password@127.0.0.1:5432/postgres?sslmode=disable",
		"postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable",
		"host=127.0.0.1 port=5432 user=postgres dbname=postgres sslmode=disable",
	} {
		try("pgx", dsn)
	}

	fmt.Println("\n== mysql candidates ==")
	for _, dsn := range []string{
		"root:root@tcp(127.0.0.1:3306)/",
		"root:password@tcp(127.0.0.1:3306)/",
		"root@tcp(127.0.0.1:3306)/",
		"mysql:root@tcp(127.0.0.1:3306)/",
	} {
		try("mysql", dsn)
	}
}
