package client_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/jasonjiang8866/smbfileserver/client"
	"github.com/jasonjiang8866/smbfileserver/smbhttp"
)

func ExampleNew() {
	// The embedding application supplies credentials from its secret mechanism.
	c, err := client.New(client.Config{Host: "files.example.test", Username: "reader", Password: "example-only", MaxFileBytes: 8 << 20, MaxConcurrentReads: 4})
	if err != nil {
		panic(err)
	}
	h, err := smbhttp.New(c, smbhttp.Options{RequestTimeout: 10 * time.Second})
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/files/", http.StripPrefix("/files", h))
	// Install authentication/authorization middleware and serve mux in your server.
	// Construction performs no network I/O; no Fiber or process startup is needed.
	fmt.Println("client and handler configured")
	// Output: client and handler configured
}

func ExampleClient_ReadFile() {
	c, err := client.New(client.Config{Host: "files.example.test", Username: "reader", Password: "example-only"})
	if err != nil {
		panic(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, err := c.ReadFile(ctx, "reports", "nested/report.csv")
	if errors.Is(err, client.ErrTooLarge) {
		fmt.Println("file exceeds configured limit")
		return
	}
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Printf("read %d bytes\n", len(data))
}

func ExampleClient_Open() {
	c, err := client.New(client.Config{Host: "files.example.test", Username: "reader", Password: "example-only"})
	if err != nil {
		panic(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	file, err := c.Open(ctx, "reports", "nested/report.csv")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer file.Close()
	n, err := io.Copy(io.Discard, file)
	if err != nil {
		fmt.Println("incomplete transfer:", err)
		return
	}
	fmt.Printf("transferred %d bytes\n", n)
}
