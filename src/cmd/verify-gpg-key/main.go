package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/mail"
	"os"
	"regexp"
	"time"

	"github.com/ProtonMail/gopenpgp/v2/crypto"

	"github.com/opentofu/registry-stable/internal/files"
	"github.com/opentofu/registry-stable/internal/github"
	"github.com/opentofu/registry-stable/internal/gpg"
	"github.com/opentofu/registry-stable/pkg/verification"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	keyFile := flag.String("key-file", "", "Location of the GPG key to verify")
	username := flag.String("username", "", "Github username to verify the GPG key against")
	orgName := flag.String("org", "", "Github organization name to verify the GPG key against")
	outputFile := flag.String("output", "", "Path to write JSON result to")
	flag.Parse()

	logger = logger.With(slog.String("github", *username), slog.String("org", *orgName))
	slog.SetDefault(logger)
	logger.Debug("Verifying GPG key from location", slog.String("location", *keyFile))

	token, err := github.EnvAuthToken()
	if err != nil {
		logger.Error("Initialization Error", slog.Any("err", err))
		os.Exit(1)
	}

	ctx, _ := context.WithTimeout(context.Background(), 10*time.Second)
	ghClient := github.NewClient(ctx, logger, token)

	result := &verification.Result{}

	s := VerifyKey(ctx, *keyFile, *orgName)
	result.Steps = append(result.Steps, s)

	s = VerifyGithubUser(ghClient, *username, *orgName)
	result.Steps = append(result.Steps, s)

	fmt.Println(result.RenderMarkdown())

	if *outputFile != "" {
		jsonErr := files.SafeWriteObjectToJSONFile(*outputFile, result.RenderMarkdown())
		if jsonErr != nil {
			// This really should not happen
			panic(jsonErr)
		}
	}

	if result.DidFail() {
		os.Exit(-1)
	}
}

func VerifyGithubUser(client github.Client, username string, orgName string) *verification.Step {
	verifyStep := &verification.Step{
		Name: "Validate Github user",
	}

	s := verifyStep.RunStep(fmt.Sprintf("User is a member of the organization %s", orgName), func() error {
		member, err := client.IsUserInOrganization(username, orgName)
		if err != nil {
			return fmt.Errorf("failed to get user: %w", err)
		}
		if member {
			return nil
		} else {
			return fmt.Errorf("user is not a member of the organization")
		}
	})
	s.Remarks = []string{"If this is incorrect, please ensure that your organization membership is public. For more information, see [Github Docs - Publicizing or hiding organization membership](https://docs.github.com/en/account-and-profile/setting-up-and-managing-your-personal-account-on-github/managing-your-membership-in-organizations/publicizing-or-hiding-organization-membership)"}

	return verifyStep
}

var gpgNameEmailRegex = regexp.MustCompile(`.*\<(.*)\>`)

func VerifyKey(ctx context.Context, location string, orgName string) *verification.Step {
	verifyStep := &verification.Step{
		Name: "Validate GPG key",
	}

	// read the key from the filesystem
	data, err := os.ReadFile(location)
	if err != nil {
		verifyStep.AddError(fmt.Errorf("failed to read key file: %w", err))
		verifyStep.Status = verification.StatusFailure
		return verifyStep
	}

	var key *crypto.Key
	verifyStep.RunStep("Key is a valid PGP key", func() error {
		k, err := gpg.ParseKey(string(data))
		if err != nil {
			return fmt.Errorf("could not parse key: %w", err)
		}
		key = k
		return nil
	})

	if key == nil {
		// The previous step failed.
		return verifyStep
	}

	verifyStep.RunStep("Key is not expired", func() error {
		if key.IsExpired() {
			return fmt.Errorf("key is expired")
		}
		return nil
	})

	verifyStep.RunStep("Key is not revoked", func() error {
		if key.IsRevoked() {
			return fmt.Errorf("key is revoked")
		}
		return nil
	})

	verifyStep.RunStep("Key can be used for signing", func() error {
		if !key.CanVerify() {
			return fmt.Errorf("key cannot be used for signing")
		}
		return nil
	})

	emailStep := verifyStep.RunStep("Key has a valid identity and email. (Email is preferable but optional)", func() error {
		if key.GetFingerprint() == "" {
			return fmt.Errorf("key has no fingerprint")
		}

		entity := key.GetEntity()
		if entity == nil {
			return fmt.Errorf("key has no entity")
		}

		identities := entity.Identities
		if len(identities) == 0 {
			return fmt.Errorf("key has no identities")
		}

		for idName, identity := range identities {
			if identity.Name == "" {
				return fmt.Errorf("key identity %s has no name", idName)
			}

			email := gpgNameEmailRegex.FindStringSubmatch(identity.Name)
			if len(email) != 2 {
				return fmt.Errorf("key identity %s has no email", idName)
			}

			_, err := mail.ParseAddress(email[1])
			if err != nil {
				return fmt.Errorf("key identity %s has an invalid email: %w", idName, err)
			}
		}

		return nil
	})

	verifyStep.RunStep("Key is used to sign the provider", func() error {
		if err := verifyKeyInProviders(ctx, key, orgName); err != nil {
			return fmt.Errorf("key is not used to sign the provider: %w", err)
		}
		return nil
	})

	emailStep.FailureToWarning()

	return verifyStep
}
