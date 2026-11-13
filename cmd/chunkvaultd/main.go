// Command chunkvaultd serves the ChunkVault REST API.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/karthik-258-code/Chunk-Vault/internal/api"
	"github.com/karthik-258-code/Chunk-Vault/internal/store"
	"github.com/karthik-258-code/Chunk-Vault/internal/vault"
)

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := rootCmd().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		stop()
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	var (
		repo       string
		addr       string
		passphrase string
		apiKey     string
		s3Bucket   string
		s3Region   string
		s3Endpoint string
	)

	cmd := &cobra.Command{
		Use:     "chunkvaultd",
		Short:   "ChunkVault REST API server",
		Version: version,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var opts []vault.Option
			if passphrase != "" {
				p := passphrase
				opts = append(opts, vault.WithPassphrase(func() ([]byte, error) {
					return []byte(p), nil
				}))
			}

			// Auto-init the repo if config.json doesn't exist yet.
			if _, serr := os.Stat(filepath.Join(repo, "config.json")); os.IsNotExist(serr) {
				log.Printf("repository %s not initialized, running init", repo)
				if ierr := vault.Init(repo); ierr != nil {
					return fmt.Errorf("auto-init: %w", ierr)
				}
			}

			var v *vault.Vault
			var err error
			if s3Bucket != "" {
				blobs, berr := store.NewS3(cmd.Context(), s3Bucket, s3Region, s3Endpoint)
				if berr != nil {
					return berr
				}
				v, err = vault.OpenWithStore(repo, blobs, opts...)
			} else {
				v, err = vault.Open(repo, opts...)
			}
			if err != nil {
				return err
			}
			defer v.Close()

			handler := api.NewRouter(v, apiKey)
			srv := &http.Server{Addr: addr, Handler: handler}

			go func() {
				<-cmd.Context().Done()
				shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = srv.Shutdown(shutCtx)
			}()

			log.Printf("chunkvaultd listening on %s (repo=%s)", addr, repo)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&repo, "repo", "./.chunkvault", "path to the chunkvault repository")
	cmd.Flags().StringVar(&addr, "addr", ":8080", "listen address")
	cmd.Flags().StringVar(&passphrase, "passphrase", os.Getenv("CHUNKVAULT_PASSPHRASE"), "repository passphrase (or use CHUNKVAULT_PASSPHRASE env)")
	cmd.Flags().StringVar(&apiKey, "api-key", os.Getenv("CHUNKVAULT_API_KEY"), "API key for authentication (or use CHUNKVAULT_API_KEY env)")
	cmd.Flags().StringVar(&s3Bucket, "s3-bucket", "", "S3 bucket name (uses S3 backend when set)")
	cmd.Flags().StringVar(&s3Region, "s3-region", "us-east-1", "AWS region for S3 backend")
	cmd.Flags().StringVar(&s3Endpoint, "s3-endpoint", "", "S3-compatible endpoint URL")

	return cmd
}
