//go:build ignore

package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

func main() {
	db := flag.String("db", "", "path to data.db")
	key := flag.String("key", "", "path to .encryption_key")
	flag.Parse()
	s, err := store.Open(*db)
	if err != nil {
		panic(err)
	}
	defer s.Close()
	c, err := credentials.Open(*key)
	if err != nil {
		panic(err)
	}
	for _, name := range []string{"openobserve_admin"} {
		cr, err := s.GetCredential(context.Background(), name)
		if err != nil {
			fmt.Printf("%s: ERR %v\n", name, err)
			continue
		}
		pw, err := c.Decrypt(cr.PasswordCiphertext)
		if err != nil {
			pw = []byte("<decrypt-fail>")
		}
		fmt.Printf("%s: user=%q pass=%q swarm=%s rotated=%v\n", name, cr.Username, string(pw), cr.SwarmSecretName, cr.RotatedAt)
	}
}
