package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/VikashLoomba/Portal/internal/app"
	"github.com/VikashLoomba/Portal/internal/clip"
	"github.com/VikashLoomba/Portal/internal/clipupload"
	"github.com/VikashLoomba/Portal/pkg/transport"
)

// clipUploadReachable gates the live clip upload on transport liveness: Ensure
// brings the master/connection up, then liveness gates on Health.Up (per T2 —
// NOT on Pid, which is 0 for the native transport). Extracted from the RunE
// closure so the gate is unit-testable without a live upload.
func clipUploadReachable(ctx context.Context, tr transport.Transport) bool {
	_, _ = tr.Ensure(ctx)
	h, _ := tr.Health(ctx)
	return h.Up
}

// newClipCheckCmd diagnoses the clipboard-paste pipeline: what's on the
// clipboard, whether portal detects an image, and (optionally) whether it
// uploads to the configured dev box.
func newClipCheckCmd(a *app.App) *cobra.Command {
	var doUpload bool
	cmd := &cobra.Command{
		Use:   "clip-check",
		Short: "Diagnose clipboard image detection (and optionally test upload)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cb := clip.New()
			fmt.Println(cb.Describe())
			fmt.Printf("\nHasImage() => %v\n", cb.HasImage())

			if !cb.HasImage() {
				fmt.Println("\nNo image detected. Copy a screenshot (Cmd+Shift+Ctrl+4) or")
				fmt.Println("copy an image from a browser/Preview, then re-run.")
				return nil
			}

			png, err := cb.ImagePNG(cmd.Context())
			if err != nil {
				fmt.Printf("\nImagePNG() failed: %v\n", err)
				return nil
			}
			fmt.Printf("ImagePNG() => %d bytes (PNG header: %x)\n", len(png), pngHead(png))

			if !doUpload {
				fmt.Println("\nLooks good. Re-run with --upload to test the full upload to your dev box.")
				return nil
			}

			host, _ := a.Cfg.ReadHost()
			if host == "" {
				fmt.Println("\nNo dev box configured; cannot test upload.")
				return nil
			}
			if !clipUploadReachable(cmd.Context(), a.Transport) {
				fmt.Printf("\nCould not reach %s to test upload.\n", host)
				return nil
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			path, err := clipupload.Upload(ctx, a.Transport, png)
			if err != nil {
				fmt.Printf("\nUpload failed: %v\n", err)
				return nil
			}
			fmt.Printf("\nUploaded OK -> %s\n", path)
			return nil
		},
	}
	cmd.Flags().BoolVar(&doUpload, "upload", false, "also test uploading the image to the configured dev box")
	return cmd
}

func pngHead(b []byte) []byte {
	if len(b) >= 8 {
		return b[:8]
	}
	return b
}
