package main

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dlclark/regexp2"
	"github.com/go-git/go-git/v5"
	"gopkg.in/yaml.v3"
)

const temp = "temp"

func init() {
	log.SetFlags(log.Lshortfile)
	log.SetOutput(os.Stdout)
}

func main() {
	log.Println("load fork config from .fork/fork.yml")
	v, err := ioutil.ReadFile(".fork/fork.yml")
	breakError(err, "load fork config fail: ")

	var opt = Options{}
	breakError(yaml.Unmarshal(v, &opt), "unmarshal form config fail:")

	if opt.Source == "" {
		log.Println("no source in .fork/fork.yml")
		os.Exit(1)
	}

	log.Println("source:", opt.Source)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	Run(ctx, opt)

	log.Println("finished.")
}

type Handler func(context.Context, []byte) []byte

type HandlerModel struct {
	Type string   `json:"type" yaml:"type"`
	Args []string `json:"args" yaml:"args"`
}

type Options struct {
	Source      string         `json:"source" yaml:"source" toml:"source"`
	BeforeShell []string       `json:"before_shell" yaml:"before_shell" toml:"before_shell"`
	ProcessFile []HandlerModel `json:"process_file" yaml:"process_file" toml:"process_file"`
	AfterShell  []string       `json:"after_shell" yaml:"after_shell" toml:"after_shell"`
	Include     []string       `json:"include" yaml:"include" toml:"include"`
	Exclude     []string       `json:"exclude" yaml:"exclude" toml:"exclude"`
}

func Run(ctx context.Context, opt Options) {
	var handlers []Handler
	for _, model := range opt.ProcessFile {
		switch model.Type {
		case "regexp":
			if len(model.Args) == 2 {
				handlers = append(handlers, rReplace(model.Args[0], model.Args[1]))
			}
		case "replace":
			if len(model.Args) == 2 {
				handlers = append(handlers, sReplace(model.Args[0], model.Args[1]))
			}
		}
	}

	breakError(clean())

	repo, err := git.PlainClone(temp, false, &git.CloneOptions{URL: opt.Source, Depth: 1, Progress: os.Stdout})
	breakError(err)
	commits, err := repo.CommitObjects()
	breakError(err)
	commit, err := commits.Next()
	breakError(err)

	var forkTxt = &bytes.Buffer{}
	fmt.Fprintln(forkTxt, "## fork by ghfork tool")
	fmt.Fprintln(forkTxt, "### fork from:")
	fmt.Fprintf(forkTxt, "    %s\n", opt.Source)
	fmt.Fprintln(forkTxt, "### fork time:")
	fmt.Fprintf(forkTxt, "    %s\n", formatTime(time.Now()))
	fmt.Fprintln(forkTxt, "### last commit:")
	fmt.Fprintf(forkTxt, "    ")
	fmt.Fprintf(forkTxt, "%s <%s> %s\n\n", commit.Author.Name, commit.Author.Email, formatTime(commit.Author.When))
	breakError(writeFile("fork.txt", forkTxt.Bytes(), 0644))

	for _, s := range opt.BeforeShell {
		breakError(bashRun(ctx, s))
	}

	breakError(filepath.Walk(temp, func(path string, info os.FileInfo, err error) error {
		if info.Name() == ".git" {
			return filepath.SkipDir
		}

		if len(opt.Include) > 0 && !info.IsDir() {
			for _, s := range opt.Include {
				if ok, _ := regexp2.MustCompile(s, 0).MatchString(path); ok {
					goto FindExclude
				}
			}
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

	FindExclude:
		for _, s := range opt.Exclude {
			if ok, _ := regexp2.MustCompile(s, 0).MatchString(path); ok {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}

		if info.IsDir() {
			return nil
		}

		target := strings.TrimPrefix(path, temp+"/")
		log.Println(path, " -> ", target)
		return handle(ctx, path, target, handlers...)
	}))

	rdme := "README.md"
	_, err = os.Stat(rdme)
	if os.IsNotExist(err) {
		writeFile(rdme, forkTxt.Bytes(), 0666)
	} else {
		rdf, _ := os.OpenFile(rdme, os.O_APPEND|os.O_RDWR, 0)
		if rdf != nil {
			rdf.Write(forkTxt.Bytes())
			rdf.Close()
		}
	}

	breakError(os.RemoveAll(temp))

	for _, s := range opt.AfterShell {
		breakError(bashRun(ctx, s))
	}
}

func breakError(err error, msg ...interface{}) {
	if err != nil {
		if len(msg) > 0 {
			log.Output(2, fmt.Sprint(append(msg, err)))
		} else {
			log.Output(2, err.Error())
		}
		os.Exit(1)
	}
}

func formatTime(t time.Time) string {
	if !t.IsZero() {
		return t.In(time.FixedZone("CST", 3600*8)).Format(time.RFC3339) //"2006-01-02 15:04:05Z07:00"
	}
	return ""
}

func handle(ctx context.Context, src, target string, handlers ...Handler) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}
	if srcInfo.Size() < 1024*20 {
		s, err := ioutil.ReadFile(src)
		if err != nil {
			return err
		}

		for _, handler := range handlers {
			s = handler(ctx, s)
		}

		if err := writeFile(target, s, srcInfo.Mode()); err != nil {
			return err
		}
		return os.Remove(src)
	}

	return moveFile(src, target)
}

func rReplace(exp, rep string) Handler {
	return func(_ context.Context, s []byte) []byte {
		r, _ := regexp2.MustCompile(exp, 0).Replace(string(s), rep, 0, -1)
		return []byte(r)
	}
}

func sReplace(old, new string) Handler {
	return func(_ context.Context, s []byte) []byte {
		return bytes.ReplaceAll(s, []byte(old), []byte(new))
	}
}

func bashRun(ctx context.Context, shellScript string) error {
	log.Println(shellScript)
	c := exec.CommandContext(ctx, "bash", "-c", shellScript)
	c.Stdout = os.Stdout
	c.Stderr = os.Stdout
	c.Stdin = os.Stdin
	return c.Run()
}

func writeFile(target string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return err
	}
	return ioutil.WriteFile(target, data, mode)
}

func moveFile(source, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return err
	}
	return os.Rename(source, target)
}

func clean() error {
	names, err := readDirNames(".")
	if err != nil {
		return err
	}
	for _, name := range names {
		if strings.HasSuffix(name, ".git") || strings.HasSuffix(name, ".fork") {
			continue
		}
		if err := os.RemoveAll(name); err != nil {
			return err
		}
	}
	return nil
}

func readDirNames(dirname string) ([]string, error) {
	f, err := os.Open(dirname)
	if err != nil {
		return nil, err
	}
	names, err := f.Readdirnames(-1)
	f.Close()
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}
