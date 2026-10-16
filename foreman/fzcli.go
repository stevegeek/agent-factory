package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/stevegeek/agent-factory/foreman/fizz"
)

// Project boards from the command line: `foreman boards …` and `foreman tokens …`.

const boardsUsage = `usage:
  foreman boards init [--account 6182510] [--name NAME] [--owner Stephen] [--email ADDR]
  foreman boards import --from-fizzy --token-file PATH [--account 6182510] [--board ID ...] [--fizzy-url https://app.fizzy.do]
  foreman tokens add NAME [--user Stephen] [--account ID]
  foreman tokens list
  foreman tokens revoke NAME
every command takes --root (default $FACTORY_ROOT or ~/factory); the store is <root>/boards`

func storeDir(root string) string { return filepath.Join(root, "boards") }

func defaultRoot() string {
	home, _ := os.UserHomeDir()
	return envOr("FACTORY_ROOT", filepath.Join(home, "factory"))
}

// parse reads flags given before or after the positional arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// runCLI runs a `boards` or `tokens` command; the exit code it returns is the process's.
func runCLI(args []string, stdout, stderr io.Writer) int {
	if err := cli(args, stdout, stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 2
		}
		fmt.Fprintln(stderr, "foreman:", err)
		return 1
	}
	return 0
}

func cli(args []string, stdout, stderr io.Writer) error {
	if len(args) < 2 {
		fmt.Fprintln(stderr, boardsUsage)
		return flag.ErrHelp
	}
	fs := flag.NewFlagSet("foreman "+args[0]+" "+args[1], flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", defaultRoot(), "factory root (FACTORY_ROOT)")
	account := fs.String("account", "", "account number, as in .fizzy.yaml")
	switch args[0] + " " + args[1] {
	case "boards init":
		name := fs.String("name", "Projects", "the account's name")
		owner := fs.String("owner", "Stephen", "the owner's name")
		email := fs.String("email", "", "the owner's email address")
		if _, err := parse(fs, args[2:]); err != nil {
			return err
		}
		st := fizz.Open(storeDir(*root))
		a, err := st.Init(orStr(*account, "6182510"), *name, *owner, *email)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "account %s (%s) in %s; owner %s\n", a.ID, a.Name, st.Dir, a.Users[0].Name)
		return nil

	case "boards import":
		from := fs.Bool("from-fizzy", false, "import from a Fizzy server (the only source)")
		tokenFile := fs.String("token-file", "", "a file holding the Fizzy API token")
		base := fs.String("fizzy-url", "https://app.fizzy.do", "the Fizzy server")
		var boards list
		fs.Var(&boards, "board", "a board id to import (repeatable; default every board)")
		if _, err := parse(fs, args[2:]); err != nil {
			return err
		}
		if !*from || *tokenFile == "" {
			return errors.New("import needs --from-fizzy and --token-file")
		}
		data, err := os.ReadFile(*tokenFile)
		if err != nil {
			return err
		}
		tok := strings.TrimSpace(string(data))
		if tok == "" {
			return fmt.Errorf("%s is empty", *tokenFile)
		}
		im := &fizz.Importer{Base: *base, Token: tok, Account: orStr(*account, "6182510"), Boards: boards,
			Log: func(f string, a ...any) { fmt.Fprintf(stderr, f+"\n", a...) }}
		n, err := im.Run(fizz.Open(storeDir(*root)))
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "imported %d boards, %d cards, %d comments; %d new users, %d new tags\n", n.Boards, n.Cards, n.Comments, n.Users, n.Tags)
		return nil

	case "tokens add":
		user := fs.String("user", "", "the user it acts as (made if new; default the owner)")
		pos, err := parse(fs, args[2:])
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return errors.New("usage: foreman tokens add NAME [--user NAME] [--account ID]")
		}
		st := fizz.Open(storeDir(*root))
		a, err := oneAccount(st, *account)
		if err != nil {
			return err
		}
		var u *fizz.User
		if *user == "" {
			for _, x := range a.Users {
				if x.Role == "owner" {
					u = x
					break
				}
			}
		} else if u = a.UserNamed(*user); u == nil {
			u = &fizz.User{ID: fizz.NewID(), Name: *user, Role: "member", Active: true, CreatedAt: st.Stamp()}
			a.Users = append(a.Users, u)
			if err := st.SaveAccount(a); err != nil {
				return err
			}
			fmt.Fprintf(stderr, "made user %s (%s)\n", u.Name, u.ID)
		}
		if u == nil || u.Role == "system" {
			return errors.New("no user to act as; pass --user NAME")
		}
		secret, err := st.AddToken(pos[0], a.ID, u.ID)
		if err != nil {
			return err
		}
		fmt.Fprintf(stderr, "token %q acts as %s in account %s; it is shown once:\n", pos[0], u.Name, a.ID)
		fmt.Fprintln(stdout, secret)
		return nil

	case "tokens list":
		if _, err := parse(fs, args[2:]); err != nil {
			return err
		}
		st := fizz.Open(storeDir(*root))
		ts, err := st.Tokens()
		if err != nil {
			return err
		}
		for _, t := range ts {
			who := t.UserID
			if a, err := st.Account(t.Account); err == nil {
				if u := a.User(t.UserID); u != nil {
					who = u.Name
				}
			}
			fmt.Fprintf(stdout, "%s\taccount %s\tas %s\tsince %s\n", t.Name, t.Account, who, t.CreatedAt)
		}
		return nil

	case "tokens revoke":
		pos, err := parse(fs, args[2:])
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return errors.New("usage: foreman tokens revoke NAME")
		}
		if err := fizz.Open(storeDir(*root)).RevokeToken(pos[0]); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "revoked %s\n", pos[0])
		return nil
	}
	fmt.Fprintln(stderr, boardsUsage)
	return flag.ErrHelp
}

// oneAccount: the named account, or the store's only one.
func oneAccount(st *fizz.Store, id string) (*fizz.Account, error) {
	if id != "" {
		return st.Account(id)
	}
	as := st.Accounts()
	switch len(as) {
	case 0:
		return nil, errors.New("no account yet; run foreman boards init first")
	case 1:
		return st.Account(as[0])
	}
	return nil, fmt.Errorf("more than one account (%s); pass --account", strings.Join(as, ", "))
}

func orStr(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
