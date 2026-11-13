package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/karthik-258-code/Chunk-Vault/internal/keys"
	"github.com/karthik-258-code/Chunk-Vault/internal/store"
	"github.com/karthik-258-code/Chunk-Vault/internal/vault"
)

const (
	defaultRepo = "./.chunkvault"
	// passphraseEnv names the environment variable that supplies the passphrase
	// of an encrypted repository without prompting.
	passphraseEnv = "CHUNKVAULT_PASSPHRASE"
)

// Seams for the terminal, replaced in tests.
var (
	// terminalFD returns the file descriptor of r if it is an interactive
	// terminal.
	terminalFD = func(r io.Reader) (int, bool) {
		f, ok := r.(*os.File)
		if !ok || !term.IsTerminal(int(f.Fd())) {
			return 0, false
		}
		return int(f.Fd()), true
	}
	// readPassword reads a line from the terminal fd without echoing it.
	readPassword = term.ReadPassword
	// kdfParams are the Argon2id parameters given to new encrypted repositories.
	kdfParams = keys.DefaultKDFParams()
)

var (
	errNoPassphrase       = errors.New("no passphrase: stdin is not a terminal, set " + passphraseEnv)
	errEmptyPassphrase    = errors.New("passphrase must not be empty")
	errPassphraseMismatch = errors.New("passphrases do not match")
)

// readPassphrase returns the repository passphrase. A non-empty
// CHUNKVAULT_PASSPHRASE wins; otherwise, if stdin is a terminal, the user is
// prompted on stderr without echo, twice when confirm is set (new
// repositories). With neither it fails rather than reading a passphrase from
// piped data. The caller should wipe the result.
func readPassphrase(cmd *cobra.Command, confirm bool) ([]byte, error) {
	if p := os.Getenv(passphraseEnv); p != "" {
		return []byte(p), nil
	}
	fd, ok := terminalFD(cmd.InOrStdin())
	if !ok {
		return nil, errNoPassphrase
	}
	errw := cmd.ErrOrStderr()
	fmt.Fprint(errw, "Passphrase: ")
	p, err := readPassword(fd)
	fmt.Fprintln(errw)
	if err != nil {
		return nil, fmt.Errorf("read passphrase: %w", err)
	}
	if len(p) == 0 {
		return nil, errEmptyPassphrase
	}
	if !confirm {
		return p, nil
	}
	defer clear(p)
	fmt.Fprint(errw, "Confirm passphrase: ")
	again, err := readPassword(fd)
	fmt.Fprintln(errw)
	if err != nil {
		return nil, fmt.Errorf("read passphrase: %w", err)
	}
	defer clear(again)
	if !bytes.Equal(p, again) {
		return nil, errPassphraseMismatch
	}
	return append([]byte(nil), p...), nil
}

// passphraseOption makes the vault ask for the passphrase, if the repository
// is encrypted.
func passphraseOption(cmd *cobra.Command) vault.Option {
	return vault.WithPassphrase(func() ([]byte, error) {
		return readPassphrase(cmd, false)
	})
}

// s3Flags holds the S3 backend flags shared across all commands.
type s3Flags struct {
	bucket   string
	region   string
	endpoint string
}

// newRootCmd builds the command tree, writing normal output to stdout and
// diagnostics to stderr.
func newRootCmd(stdout, stderr io.Writer) *cobra.Command {
	var repo string
	var s3f s3Flags

	root := &cobra.Command{
		Use:           "chunkvault",
		Short:         "A content-addressable, deduplicating file store",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.PersistentFlags().StringVar(&repo, "repo", defaultRepo, "path to the chunkvault repository")
	root.PersistentFlags().StringVar(&s3f.bucket, "s3-bucket", "", "S3 bucket name (uses S3 backend when set)")
	root.PersistentFlags().StringVar(&s3f.region, "s3-region", "us-east-1", "AWS region for S3 backend")
	root.PersistentFlags().StringVar(&s3f.endpoint, "s3-endpoint", "", "S3-compatible endpoint URL (e.g. http://localhost:9000)")

	root.AddCommand(
		newInitCmd(),
		newPutCmd(&repo, &s3f),
		newGetCmd(&repo, &s3f),
		newLsCmd(&repo, &s3f),
		newRmCmd(&repo, &s3f),
		newReindexCmd(&repo, &s3f),
		newVerifyCmd(&repo, &s3f),
		newStatsCmd(&repo, &s3f),
		newGCCmd(&repo, &s3f),
		newSnapshotCmd(&repo, &s3f),
	)
	return root
}

// openVaultS3 opens a vault, using the S3 backend when --s3-bucket is set,
// otherwise falling back to the local filesystem backend.
func openVaultS3(cmd *cobra.Command, repo string, s3f *s3Flags) (*vault.Vault, error) {
	if s3f.bucket == "" {
		return vault.Open(repo, passphraseOption(cmd))
	}
	blobs, err := store.NewS3(cmd.Context(), s3f.bucket, s3f.region, s3f.endpoint)
	if err != nil {
		return nil, err
	}
	return vault.OpenWithStore(repo, blobs, passphraseOption(cmd))
}

func newInitCmd() *cobra.Command {
	var encrypt bool
	cmd := &cobra.Command{
		Use:   "init [--encrypt] <dir>",
		Short: "Create a new repository in <dir>",
		Long: "Create a new repository in <dir>. Blobs are compressed with zstd.\n" +
			"With --encrypt they are also encrypted with AES-256-GCM under a random master\n" +
			"key that is wrapped with a key derived from a passphrase (Argon2id). The\n" +
			"passphrase is read twice without echo when stdin is a terminal, or taken from\n" +
			"the " + passphraseEnv + " environment variable when that is set. The passphrase\n" +
			"is never stored and cannot be recovered: lose it and the data is gone.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var opts []vault.InitOption
			if encrypt {
				pass, err := readPassphrase(cmd, true)
				if err != nil {
					return err
				}
				defer clear(pass)
				opts = append(opts, vault.InitEncrypted(pass), vault.InitKDFParams(kdfParams))
			}
			if err := vault.Init(args[0], opts...); err != nil {
				return err
			}
			mode := "unencrypted"
			if encrypt {
				mode = "encrypted"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Initialized empty %s chunkvault repository in %s\n", mode, args[0])
			return nil
		},
	}
	cmd.Flags().BoolVar(&encrypt, "encrypt", false, "encrypt the repository with a passphrase")
	return cmd
}

func newPutCmd(repo *string, s3f *s3Flags) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "put <file>",
		Short: "Store a file, deduplicating against existing chunks",
		Long: "Store a file in the repository. The file is split into content-defined\n" +
			"chunks; chunks already present in the repository are not stored again.\n" +
			"Use '-' as <file> to read from standard input (requires --name).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			src := args[0]
			var in io.Reader
			if src == "-" {
				if name == "" {
					return errors.New("--name is required when reading from standard input")
				}
				in = cmd.InOrStdin()
			} else {
				f, err := os.Open(src)
				if err != nil {
					if errors.Is(err, fs.ErrNotExist) {
						return fmt.Errorf("%s: no such file", src)
					}
					return err
				}
				defer f.Close()
				if st, err := f.Stat(); err != nil {
					return err
				} else if st.IsDir() {
					return fmt.Errorf("%s is a directory", src)
				}
				in = f
				if name == "" {
					name = filepath.Base(src)
				}
			}

			v, err := openVaultS3(cmd, *repo, s3f)
			if err != nil {
				return err
			}
			defer v.Close()
			start := time.Now()
			stats, err := v.Put(cmd.Context(), name, in)
			if err != nil {
				return err
			}
			printPutStats(cmd.OutOrStdout(), name, stats, time.Since(start))
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "name to store the file under (default: the file's base name)")
	return cmd
}

func printPutStats(w io.Writer, name string, s vault.PutStats, elapsed time.Duration) {
	fmt.Fprintf(w, "Stored %q (%s) in %s\n", name, humanBytes(s.TotalBytes), elapsed.Round(time.Millisecond))
	fmt.Fprintf(w, "  manifest:      %s\n", s.ManifestKey)
	fmt.Fprintf(w, "  total chunks:  %d\n", s.TotalChunks)
	fmt.Fprintf(w, "  new chunks:    %d (%s)\n", s.NewChunks, humanBytes(s.NewBytes))
	fmt.Fprintf(w, "  deduplicated:  %d chunks (%s", s.TotalChunks-s.NewChunks, humanBytes(s.DedupBytes))
	if s.TotalBytes > 0 {
		fmt.Fprintf(w, ", %.1f%%", 100*float64(s.DedupBytes)/float64(s.TotalBytes))
	}
	fmt.Fprintln(w, ")")
}

func newGetCmd(repo *string, s3f *s3Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "get <name> <outfile>",
		Short: "Restore a stored file to <outfile>",
		Long: "Reassemble a stored file, verifying the hash of every chunk, and write it\n" +
			"to <outfile>. Use '-' as <outfile> to write to standard output.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, out := args[0], args[1]
			v, err := openVaultS3(cmd, *repo, s3f)
			if err != nil {
				return err
			}
			defer v.Close()
			if out == "-" {
				return v.Get(cmd.Context(), name, cmd.OutOrStdout())
			}
			return restoreToFile(cmd, v, name, out)
		},
	}
}

// restoreToFile writes to a temporary file next to out and renames it into
// place only after the whole file has been verified, so a corrupt or partial
// restore never leaves a truncated outfile behind.
func restoreToFile(cmd *cobra.Command, v *vault.Vault, name, out string) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(out), ".chunkvault-get-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if err = v.Get(cmd.Context(), name, tmp); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Chmod(tmp.Name(), outputMode(out)); err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), out); err != nil {
		return err
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Restored %q to %s\n", name, out)
	return nil
}

// outputMode returns the permission bits for a restored file: those of an
// existing out, so overwriting a file does not change its mode, or 0o644 for a
// new file. CreateTemp makes files 0o600, which would be too strict for new
// ones.
func outputMode(out string) os.FileMode {
	if st, err := os.Stat(out); err == nil && st.Mode().IsRegular() {
		return st.Mode().Perm()
	}
	return 0o644
}

func newLsCmd(repo *string, s3f *s3Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List stored files",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			v, err := openVaultS3(cmd, *repo, s3f)
			if err != nil {
				return err
			}
			defer v.Close()
			entries, err := v.List(cmd.Context())
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tSIZE\tCHUNKS\tCREATED\tMANIFEST")
			for _, e := range entries {
				fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%.12s\n",
					e.Name, humanBytes(e.Size), e.Chunks, e.Created.Format(time.RFC3339), e.ManifestKey)
			}
			return tw.Flush()
		},
	}
}

func newRmCmd(repo *string, s3f *s3Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a stored file",
		Long: "Remove the file stored as <name>: its name is deleted and the index releases\n" +
			"the chunks it used. The chunk data stays on disk, also chunks that no file uses\n" +
			"any more, until 'chunkvault gc' deletes them, so rm does not free disk space immediately. Other files that share chunks with <name> are not\n" +
			"affected.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVaultS3(cmd, *repo, s3f)
			if err != nil {
				return err
			}
			defer v.Close()
			if err := v.Remove(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed %q (chunk data stays on disk until garbage collection)\n", args[0])
			return nil
		},
	}
}

func newReindexCmd(repo *string, _ *s3Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "reindex",
		Short: "Rebuild the index from the stored files",
		Long: "Rebuild index.db from the refs and the manifests they point at, in a temporary\n" +
			"file that is renamed over index.db only when complete. Use it to create the\n" +
			"index of a repository that has none, or to repair one after a crash or\n" +
			"damage: the index is only a cache, so no data is lost. Do not run other\n" +
			"chunkvault commands on the repository at the same time. An encrypted\n" +
			"repository needs its passphrase.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := vault.Reindex(cmd.Context(), *repo, passphraseOption(cmd))
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Reindexed %s: %d files, %d chunks\n", *repo, st.Files, st.Chunks)
			return nil
		},
	}
}

func newVerifyCmd(repo *string, s3f *s3Flags) *cobra.Command {
	var full bool
	cmd := &cobra.Command{
		Use:   "verify [--full]",
		Short: "Check the repository for missing, corrupt or inconsistent data",
		Long: "Check that every stored file's manifest and chunks exist and that the index\n" +
			"agrees with them: reference counts are recomputed from the manifests and\n" +
			"compared with the index, and stored sizes with the blobs on disk. Every\n" +
			"problem found is listed, not just the first, and the exit status is non-zero\n" +
			"if there is any. A size mismatch usually means a damaged blob, not a stale\n" +
			"index, so reindex is not suggested for it; check with --full.\n\n" +
			"By default chunk blobs are not read. With --full every referenced chunk is\n" +
			"also read, decrypted, decompressed and re-hashed, which finds damaged blobs\n" +
			"but reads the whole repository. An encrypted repository needs its passphrase.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			v, err := openVaultS3(cmd, *repo, s3f)
			if err != nil {
				return err
			}
			defer v.Close()
			rep, err := v.Verify(cmd.Context(), full)
			if err != nil {
				return err
			}
			return printVerifyReport(cmd.OutOrStdout(), rep)
		},
	}
	cmd.Flags().BoolVar(&full, "full", false, "also read, decrypt, decompress and re-hash every chunk")
	return cmd
}

// errVerifyFailed is returned by verify when it found problems, after the
// report has been printed, so that the exit status is non-zero.
var errVerifyFailed = errors.New("verification failed")

func printVerifyReport(w io.Writer, rep vault.VerifyReport) error {
	mode := "default"
	if rep.Full {
		mode = "full"
	}
	fmt.Fprintf(w, "Checked %d files, %d chunks, %s (%s): ", rep.Files, rep.Chunks, humanBytes(rep.Bytes), mode)
	if rep.OK() {
		fmt.Fprintln(w, "no problems found")
		return nil
	}
	fmt.Fprintf(w, "%s found\n", plural(len(rep.Problems), "problem"))
	for _, p := range rep.Problems {
		fmt.Fprintf(w, "  %s\n", p)
	}
	switch {
	case rep.IndexOnly():
		fmt.Fprintln(w, "Only the index is inconsistent; run 'chunkvault reindex' to rebuild it.")
	case rep.HasSizeMismatch() && !rep.Full:
		// Not reindex: it would adopt the size of a damaged blob.
		fmt.Fprintln(w, "A blob whose size differs from the index may be truncated or damaged; run 'chunkvault verify --full' to check it.")
	}
	return fmt.Errorf("%w: %s", errVerifyFailed, plural(len(rep.Problems), "problem"))
}

// plural formats n followed by noun, which gets an "s" unless n is 1.
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func newGCCmd(repo *string, s3f *s3Flags) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "gc [--dry-run]",
		Short: "Delete unreferenced chunk blobs (garbage collection)",
		Long: "Remove chunk blobs that no stored file references, left behind by 'rm' or\n" +
			"by overwriting a file with 'put'. With --dry-run the blobs are counted and\n" +
			"sized but not deleted. An encrypted repository needs its passphrase.\n\n" +
			"Run 'chunkvault verify' first if you are unsure the index is consistent;\n" +
			"'chunkvault reindex' repairs a stale index before GC sees it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			v, err := openVaultS3(cmd, *repo, s3f)
			if err != nil {
				return err
			}
			defer v.Close()
			st, err := v.GC(cmd.Context(), dryRun)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if dryRun {
				fmt.Fprintf(w, "Would collect %s chunks (%s)\n",
					plural(int(st.Collected), "chunk"), humanBytes(st.FreedBytes))
			} else {
				fmt.Fprintf(w, "Collected %s, freed %s\n",
					plural(int(st.Collected), "chunk"), humanBytes(st.FreedBytes))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "count reclaimable blobs without deleting them")
	return cmd
}

func newStatsCmd(repo *string, s3f *s3Flags) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "stats [--json]",
		Short: "Show how much space the repository uses",
		Long: "Show the number of files and unique chunks, the logical size of the files,\n" +
			"the size stored on disk, the deduplication ratio (logical bytes over unique\n" +
			"raw bytes), the compression ratio (unique raw bytes over stored bytes) and\n" +
			"the chunks no file references any more, which 'chunkvault gc' reclaims. The figures come from the index; run\n" +
			"'chunkvault verify' to check it. With --json the output is one JSON object\n" +
			"with stable snake_case keys.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			v, err := openVaultS3(cmd, *repo, s3f)
			if err != nil {
				return err
			}
			defer v.Close()
			s, err := v.Stats(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(s)
			}
			printStats(cmd.OutOrStdout(), s)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the statistics as JSON")
	return cmd
}

func printStats(w io.Writer, s vault.Stats) {
	ratio := func(r float64) string {
		if r == 0 {
			return "n/a"
		}
		return fmt.Sprintf("%.2fx", r)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "Files:\t%d\n", s.Files)
	fmt.Fprintf(tw, "Logical size:\t%s\n", humanBytes(s.LogicalBytes))
	fmt.Fprintf(tw, "Unique chunks:\t%d\n", s.UniqueChunks)
	fmt.Fprintf(tw, "Unique raw size:\t%s\n", humanBytes(s.UniqueRawBytes))
	fmt.Fprintf(tw, "Stored size:\t%s\n", humanBytes(s.StoredBytes))
	fmt.Fprintf(tw, "Dedup ratio:\t%s\n", ratio(s.DedupRatio))
	fmt.Fprintf(tw, "Compression ratio:\t%s\n", ratio(s.CompressionRatio))
	fmt.Fprintf(tw, "Unreferenced chunks:\t%d\n", s.UnreferencedChunks)
	fmt.Fprintf(tw, "Reclaimable by GC:\t%s\n", humanBytes(s.ReclaimableBytes))
	_ = tw.Flush()
}

func newSnapshotCmd(repo *string, s3f *s3Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Manage point-in-time snapshots of the repository",
	}
	cmd.AddCommand(
		newSnapshotCreateCmd(repo, s3f),
		newSnapshotLsCmd(repo, s3f),
		newSnapshotRestoreCmd(repo, s3f),
	)
	return cmd
}

func newSnapshotCreateCmd(repo *string, s3f *s3Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "create <tag>",
		Short: "Take a snapshot of the current repository state",
		Long: "Record the current set of stored files and their manifest keys as a named\n" +
			"snapshot. The tag must be a valid file name. Blobs are not copied: a\n" +
			"snapshot only records which manifest key each file pointed at.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVaultS3(cmd, *repo, s3f)
			if err != nil {
				return err
			}
			defer v.Close()
			snap, err := v.CreateSnapshot(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Snapshot %q created at %s (%s)\n",
				snap.Tag, snap.Created.Format(time.RFC3339), plural(len(snap.Files), "file"))
			return nil
		},
	}
}

func newSnapshotLsCmd(repo *string, s3f *s3Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List snapshots",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			v, err := openVaultS3(cmd, *repo, s3f)
			if err != nil {
				return err
			}
			defer v.Close()
			snaps, err := v.ListSnapshots(cmd.Context())
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "TAG\tCREATED\tFILES")
			for _, s := range snaps {
				fmt.Fprintf(tw, "%s\t%s\t%d\n", s.Tag, s.Created.Format(time.RFC3339), len(s.Files))
			}
			return tw.Flush()
		},
	}
}

func newSnapshotRestoreCmd(repo *string, s3f *s3Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "restore <tag>",
		Short: "Restore files to the versions recorded in a snapshot",
		Long: "Restore files to the versions recorded in the named snapshot. Files whose\n" +
			"current version already matches the snapshot are left untouched. Files not\n" +
			"recorded in the snapshot are also left untouched — this is a restore, not a\n" +
			"rollback: files added after the snapshot are not deleted.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := openVaultS3(cmd, *repo, s3f)
			if err != nil {
				return err
			}
			defer v.Close()
			if err := v.RestoreFromSnapshot(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Restored from snapshot %q\n", args[0])
			return nil
		},
	}
}

// humanBytes formats n using binary prefixes.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
