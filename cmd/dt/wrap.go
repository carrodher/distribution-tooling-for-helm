package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/vmware-labs/distribution-tooling-for-helm/chartutils"
	"github.com/vmware-labs/distribution-tooling-for-helm/imagelock"
	"github.com/vmware-labs/distribution-tooling-for-helm/internal/log"
	"github.com/vmware-labs/distribution-tooling-for-helm/utils"
)

var wrapCmd = newWrapCommand()

func wrapChart(ctx context.Context, inputPath string, outputFile string, platforms []string, flags *pflag.FlagSet) error {
	parentLog := getLogger()

	// Allows silencing called methods
	silentLog := log.SilentLog

	l := parentLog.StartSection(fmt.Sprintf("Wrapping Helm chart %q", inputPath))
	chartPath, err := resolveInputChartPath(inputPath, l, flags)
	if err != nil {
		return err
	}

	chart, err := chartutils.LoadChart(chartPath)
	if err != nil {
		return fmt.Errorf("failed to load Helm chart: %w", err)
	}
	chartRoot := chart.RootDir()

	lockFile, err := getImageLockFilePath(chartPath)
	if err != nil {
		return fmt.Errorf("failed to determine Images.lock file location: %w", err)
	}
	if utils.FileExists(lockFile) {
		if err := l.ExecuteStep("Verifying Images.lock", func() error {
			return verifyLock(chartPath, lockFile)
		}); err != nil {
			return l.Failf("Failed to verify lock: %w", err)
		}
		l.Infof("Helm chart %q lock is valid", chartPath)

	} else {
		err := l.ExecuteStep(
			"Images.lock file does not exist. Generating it from annotations...",
			func() error {
				return createImagesLock(chartPath,
					lockFile, silentLog,
					imagelock.WithPlatforms(platforms),
					imagelock.WithContext(ctx),
				)
			},
		)
		if err != nil {
			return l.Failf("Failed to generate lock: %w", err)
		}
		l.Infof("Images.lock file written to %q", lockFile)
	}
	if outputFile == "" {
		outputBaseName := fmt.Sprintf("%s-%s.wrap.tgz", chart.Name(), chart.Metadata.Version)
		if outputFile, err = filepath.Abs(outputBaseName); err != nil {
			l.Debugf("failed to normalize output file: %v", err)
			outputFile = filepath.Join(filepath.Dir(chartRoot), outputBaseName)
		}
	}
	if err := l.Section(fmt.Sprintf("Pulling images into %q", chart.ImagesDir()), func(childLog log.SectionLogger) error {
		if err := pullChartImages(
			chart,
			chartutils.WithLog(childLog),
			chartutils.WithContext(ctx),
			chartutils.WithProgressBar(childLog.ProgressBar()),
		); err != nil {
			return childLog.Failf("%v", err)
		}
		childLog.Infof("All images pulled successfully")
		return nil
	}); err != nil {
		return err
	}

	if err := l.ExecuteStep(
		"Compressing Helm chart...",
		func() error {
			return compressChart(ctx, chart, outputFile)
		},
	); err != nil {
		return l.Failf("failed to wrap Helm chart: %w", err)
	}
	l.Infof("Compressed into %q", outputFile)

	l.Printf(terminalSpacer)

	parentLog.Successf("Helm chart wrapped into %q", outputFile)
	return nil
}

func newWrapCommand() *cobra.Command {
	var outputFile string
	var version string
	var platforms []string
	var examples = `  # Wrap a Helm chart from a local folder
  $ dt wrap examples/mariadb

  # Wrap a Helm chart in an OCI registry
  $ dt wrap oci://docker.io/bitnamicharts/mariadb
	`
	cmd := &cobra.Command{
		Use:   "wrap CHART_PATH|OCI_URI",
		Short: "Wraps a Helm chart",
		Long: `Wraps a Helm chart either local or remote into a distributable package.
This command will pull all the container images and wrap it into a single tarball along with the Images.lock and metadata`,
		Example:       examples,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			chartPath := args[0]

			ctx, cancel := contextWithSigterm(context.Background())
			defer cancel()

			err := wrapChart(ctx, chartPath, outputFile, platforms, cmd.Flags())
			if err != nil {
				if _, ok := err.(*log.LoggedError); ok {
					// We already logged it, lets be less verbose
					return fmt.Errorf("failed to wrap Helm chart")
				}
				return err
			}
			return nil
		},
	}

	cmd.PersistentFlags().StringVar(&version, "version", version, "when wrapping remote Helm charts from OCI, version to request")
	cmd.PersistentFlags().StringVar(&outputFile, "output-file", outputFile, "generate a tar.gz with the output of the pull operation")
	cmd.PersistentFlags().StringSliceVar(&platforms, "platforms", platforms, "platforms to include in the Images.lock file")

	return cmd
}

func resolveInputChartPath(inputPath string, l log.SectionLogger, flags *pflag.FlagSet) (string, error) {
	var chartPath string

	tmpDir, err := getGlobalTempWorkDir()
	if err != nil {
		return "", err
	}

	if strings.HasPrefix(inputPath, "oci://") {
		if err := l.ExecuteStep("Fetching remote Helm chart", func() error {
			version, err := flags.GetString("version")
			if err != nil {
				return fmt.Errorf("failed to retrieve version flag: %w", err)
			}
			chartPath, err = fetchRemoteChart(inputPath, version, tmpDir)
			if err != nil {
				return err
			}
			return nil
		}); err != nil {
			return "", l.Failf("Failed to download Helm chart: %w", err)
		}
		l.Infof("Helm chart downloaded to %q", chartPath)
	} else if isTar, _ := utils.IsTarFile(inputPath); isTar {
		if err := l.ExecuteStep("Uncompressing Helm chart", func() error {
			var err error
			chartPath, err = untarChart(inputPath, tmpDir)
			return err
		}); err != nil {
			return "", l.Failf("Failed to uncompress %q: %w", inputPath, err)
		}
		l.Infof("Helm chart uncompressed to %q", chartPath)
	} else {
		chartPath = inputPath
	}

	return chartPath, nil
}
func fetchRemoteChart(chartURL string, version string, dir string) (string, error) {
	return utils.FetchRemoteChart(chartURL, version, dir)
}

func init() {
	rootCmd.AddCommand(wrapCmd)
}
