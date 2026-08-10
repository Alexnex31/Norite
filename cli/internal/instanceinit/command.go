package instanceinit

import (
	"context"
	"os"

	"github.com/urfave/cli/v3"
)

// dbPasswordEnvVar lets a scripted run supply the password without putting it in the command line.
//
// A password passed as --db-password is visible in shell history and, on most systems, in the process
// list of every other user on the machine for as long as the command runs. The flag stays available
// because some automation genuinely has nowhere better to put it, but the environment variable is the
// path worth pointing people at.
const dbPasswordEnvVar = "NORITE_DB_PASSWORD"

// Command builds the `norite instance init` command.
func Command() *cli.Command {
	return &cli.Command{
		Name:  "init",
		Usage: "Set up this machine's Norite instance configuration",
		Description: "Asks what a new instance needs to know and writes a config file the backend reads\n" +
			"at startup.\n\n" +
			"By default it asks only about things with no safe default — how to reach Postgres, and\n" +
			"whether registration is open — and reports what it defaulted for everything else. Use\n" +
			"--full to be asked about every setting.\n\n" +
			"Every question can be answered in advance with the matching flag. Supply them all with\n" +
			"--non-interactive for unattended provisioning; the command then never prompts and fails\n" +
			"if something required is missing.\n\n" +
			"The file it writes contains credentials and is created with 0600 permissions.",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "full",
				Usage: "ask about every setting, not only those with no safe default",
			},
			&cli.BoolFlag{
				Name:  "non-interactive",
				Usage: "never prompt; use flags and defaults only, and fail if a required value is missing",
			},
			&cli.StringFlag{
				Name:    "output",
				Aliases: []string{"o"},
				Usage:   "write the config file to `PATH` instead of the default location",
			},
			&cli.BoolFlag{
				Name:  "force",
				Usage: "replace an existing config file instead of refusing",
			},

			&cli.StringFlag{
				Name:  "env",
				Usage: "deployment `ENV`: development or production",
			},
			&cli.StringFlag{
				Name:  "listen-addr",
				Usage: "`ADDRESS` the backend listens on, e.g. :8080 or 127.0.0.1:8080",
			},

			&cli.StringFlag{
				Name:  "database-url",
				Usage: "complete Postgres connection `URL`, instead of the individual --db-* flags",
			},
			&cli.StringFlag{Name: "db-host", Usage: "Postgres `HOST`"},
			&cli.StringFlag{Name: "db-port", Usage: "Postgres `PORT`"},
			&cli.StringFlag{Name: "db-name", Usage: "database `NAME`"},
			&cli.StringFlag{Name: "db-user", Usage: "database `USER`"},
			&cli.StringFlag{
				Name:    "db-password",
				Usage:   "database `PASSWORD` — prefer " + dbPasswordEnvVar + ", which other users cannot read from the process list",
				Sources: cli.EnvVars(dbPasswordEnvVar),
			},
			&cli.StringFlag{Name: "db-sslmode", Usage: "Postgres SSL `MODE`: disable, require, verify-full, …"},

			&cli.StringFlag{Name: "storage", Usage: "attachment storage `BACKEND`: local or s3"},
			&cli.StringFlag{Name: "storage-path", Usage: "`DIRECTORY` for attachments when storage is local"},
			&cli.StringFlag{Name: "s3-endpoint", Usage: "S3-compatible service `URL` (empty for AWS S3)"},
			&cli.StringFlag{Name: "s3-region", Usage: "S3 `REGION`"},
			&cli.StringFlag{Name: "s3-bucket", Usage: "S3 bucket `NAME`"},
			&cli.StringFlag{Name: "s3-access-key-id", Usage: "S3 access key `ID`"},
			&cli.StringFlag{
				Name:    "s3-secret-access-key",
				Usage:   "S3 secret access `KEY` — prefer NORITE_S3_SECRET_ACCESS_KEY",
				Sources: cli.EnvVars("NORITE_S3_SECRET_ACCESS_KEY"),
			},
			&cli.BoolFlag{Name: "s3-force-path-style", Usage: "address buckets as endpoint/bucket"},

			&cli.BoolFlag{Name: "acme", Usage: "obtain and renew TLS certificates automatically"},
			&cli.StringFlag{Name: "acme-domain", Usage: "public `HOSTNAME` to obtain a certificate for"},
			&cli.StringFlag{Name: "acme-email", Usage: "contact `EMAIL` for certificate expiry notices"},

			&cli.StringFlag{Name: "registration", Usage: "registration `POLICY`: open or invite"},
		},
		Action: func(_ context.Context, cmd *cli.Command) error {
			opts := Options{
				Full:           cmd.Bool("full"),
				NonInteractive: cmd.Bool("non-interactive"),
				Output:         cmd.String("output"),
				Force:          cmd.Bool("force"),

				Env:        cmd.String("env"),
				ListenAddr: cmd.String("listen-addr"),

				DatabaseURL: cmd.String("database-url"),
				DBHost:      cmd.String("db-host"),
				DBPort:      cmd.String("db-port"),
				DBName:      cmd.String("db-name"),
				DBUser:      cmd.String("db-user"),
				DBPassword:  cmd.String("db-password"),
				DBSSLMode:   cmd.String("db-sslmode"),

				Storage:          cmd.String("storage"),
				StorageLocalPath: cmd.String("storage-path"),

				S3Endpoint:        cmd.String("s3-endpoint"),
				S3Region:          cmd.String("s3-region"),
				S3Bucket:          cmd.String("s3-bucket"),
				S3AccessKeyID:     cmd.String("s3-access-key-id"),
				S3SecretAccessKey: cmd.String("s3-secret-access-key"),

				ACMEDomain: cmd.String("acme-domain"),
				ACMEEmail:  cmd.String("acme-email"),

				Registration: cmd.String("registration"),
			}

			// Booleans need "not passed" kept distinct from "passed as false", so that leaving --acme off
			// falls through to the prompt while --acme=false is taken as a deliberate answer.
			if cmd.IsSet("acme") {
				v := cmd.Bool("acme")
				opts.ACME = &v
			}
			if cmd.IsSet("s3-force-path-style") {
				v := cmd.Bool("s3-force-path-style")
				opts.S3ForcePathStyle = &v
			}

			return Run(opts, os.Stdin, cmd.Writer)
		},
	}
}

// GroupCommand builds the `norite instance` command group that init lives under. Later milestones hang the
// rest of the instance-administration commands here.
func GroupCommand() *cli.Command {
	return &cli.Command{
		Name:     "instance",
		Usage:    "Administer this Norite instance",
		Commands: []*cli.Command{Command()},
	}
}
