package taskrunner

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	hclog "github.com/hashicorp/go-hclog"
	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/client/allocrunner/interfaces"
	"github.com/hashicorp/nomad/client/taskenv"
	"github.com/hashicorp/nomad/nomad/structs"
)

type nixHook struct {
	alloc   *structs.Allocation
	runner  *TaskRunner
	logger  log.Logger
	taskEnv *taskenv.TaskEnv
}

func newNixHook(runner *TaskRunner, logger log.Logger) *nixHook {
	h := &nixHook{
		alloc:  runner.Alloc(),
		runner: runner,
	}
	h.logger = logger.Named(h.Name())
	return h
}

func (*nixHook) Name() string {
	return "nix"
}

func (h *nixHook) Prestart(ctx context.Context, req *interfaces.TaskPrestartRequest, resp *interfaces.TaskPrestartResponse) error {
	h.taskEnv = req.TaskEnv
	flake, ok := req.Task.Config["flake"].(string)
	if !ok {
		return nil
	}

	return h.install(flake, req.TaskDir.Dir)
}

func (h *nixHook) install(flake string, taskDir string) error {
	h.logger.Debug("Building flake", "flake", flake)

	// First we build the derivation to make sure all paths are in the host store
	cmd := exec.Command("nix", "build", "--no-link", flake)
	nixBuildOutput, err := cmd.Output()
	h.logger.Debug("nix build --no-link", "flake", flake, "output", string(nixBuildOutput))
	if err != nil {
		return err
	}

	// Then get the path to the derivation output
	cmd = exec.Command("nix", "eval", "--raw", flake+".outPath")
	nixEvalOutput, err := cmd.Output()
	h.logger.Debug("nix eval --raw", "expr", flake+".outPath", "output", string(nixEvalOutput))
	if err != nil {
		return err
	}

	// Collect all store paths required to run it
	cmd = exec.Command("nix-store", "--query", "--requisites", string(nixEvalOutput))
	nixStoreOutput, err := cmd.Output()
	h.logger.Debug("nix-store --query --requisites", "path", string(nixEvalOutput), "output", string(nixStoreOutput))
	if err != nil {
		return err
	}

	// Now copy each dependency into the allocation directory
	storePaths := strings.Fields(string(nixStoreOutput))
	for _, storePath := range storePaths {
		h.logger.Debug("copying", "path", storePath)
		err = filepath.Walk(storePath, copyAll(h.logger, taskDir, false))
		if err != nil {
			return err
		}
	}

	h.logger.Debug("Creating top-level links...")

	// TODO: choose correct architecture, atm this only works on x86_64-linux
	// This uses the nixpkgs symlinkJoin derivation to build a directory that
	// looks like normal FHS, e.g. /bin /share /etc and the like.
	cmd = exec.Command(
		"nix", "eval", "--raw", flake, "--apply", `
			let
				pkgs = builtins.getFlake
					"github:NixOS/nixpkgs?rev=aea7242187f21a120fe73b5099c4167e12ec9aab";
			in pkg:
			let
				sym = pkgs.legacyPackages.x86_64-linux.symlinkJoin {
					name = "symlinks";
					paths = [ pkg ];
				};
			in builtins.seq (builtins.pathExists sym) sym.outPath
	`)
	symlinkOutput, err := cmd.Output()
	if err != nil {
		return err
	}

	return filepath.Walk(string(symlinkOutput), copyAll(h.logger, taskDir, true))
}

func copyAll(logger hclog.Logger, targetDir string, truncate bool) filepath.WalkFunc {
	return func(path string, info os.FileInfo, err error) error {
		logger.Debug("walking", "path", path)

		if err != nil {
			return err
		}

		var dst string
		if truncate {
			parts := splitPath(path)
			dst = filepath.Join(append([]string{targetDir}, parts[3:]...)...)
		} else {
			dst = filepath.Join(targetDir, path)
		}

		logger.Debug("calculated dst", "dst", dst)

		// Skip the file if it already exists at the dst
		stat, err := os.Lstat(dst)
		logger.Debug("stat errors?", "err", err, "stat", stat)
		if err == nil {
			return nil
		}
		if !os.IsNotExist(err) {
			return err
		}

		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			logger.Debug("l", "link", link, "dst", dst)
			if err := os.Symlink(link, dst); err != nil {
				return err
			}
			if info.IsDir() {
				return filepath.SkipDir
			} else {
				return nil
			}
		}

		if info.IsDir() {
			logger.Debug("d", "dst", dst)
			return os.MkdirAll(dst, 0777)
		}

		logger.Debug("f", "dst", dst)
		srcfd, err := os.Open(path)
		if err != nil {
			return err
		}
		defer srcfd.Close()

		dstfd, err := os.Create(dst)
		if err != nil {
			return err
		}
		defer dstfd.Close()

		if _, err = io.Copy(dstfd, srcfd); err != nil {
			return err
		}

		return os.Chmod(dst, info.Mode())
	}
}

// SplitPath splits a file path into its directories and filename.
func splitPath(path string) []string {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	if dir == "/" {
		return []string{base}
	} else {
		return append(splitPath(dir), base)
	}
}
