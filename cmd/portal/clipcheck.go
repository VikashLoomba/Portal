package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/app"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/clip"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/clipupload"
)

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

			png, err := cb.ImagePNG()
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
			if pid, _, _ := a.Transport.EnsureMaster(cmd.Context()); pid == 0 {
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
