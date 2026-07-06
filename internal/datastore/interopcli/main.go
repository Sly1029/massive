package main

import (
	"context"
	"fmt"
	"os"

	"github.com/Sly1029/massive/internal/datastore"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 4 {
		return fmt.Errorf("usage: interopcli write-local <root> <key> <body> [content-type] | read-local <root> <key>")
	}

	store, err := datastore.NewLocalDatastore(datastore.LocalConfig{Root: os.Args[2]})
	if err != nil {
		return err
	}

	key, err := datastore.ParseKey(os.Args[3])
	if err != nil {
		return err
	}

	switch os.Args[1] {
	case "write-local":
		if len(os.Args) < 5 {
			return fmt.Errorf("write-local requires body")
		}
		contentType := "text/plain"
		if len(os.Args) >= 6 {
			contentType = os.Args[5]
		}
		_, err := store.Put(context.Background(), key, []byte(os.Args[4]), datastore.PutOptions{ContentType: contentType})
		return err
	case "read-local":
		object, err := store.Get(context.Background(), key)
		if err != nil {
			return err
		}
		fmt.Printf("%s\n%s\n", object.Info.ContentType, string(object.Body))
		return nil
	default:
		return fmt.Errorf("unknown command %q", os.Args[1])
	}
}
