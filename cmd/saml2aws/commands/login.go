package commands

import (
	"encoding/base64"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/versent/saml2aws"
	"github.com/versent/saml2aws/helper/credentials"
	"github.com/versent/saml2aws/pkg/awsconfig"
	"github.com/versent/saml2aws/pkg/cfg"
	"github.com/versent/saml2aws/pkg/creds"
	"github.com/versent/saml2aws/pkg/flags"
)

// Login login to ADFS
func Login(loginFlags *flags.LoginExecFlags) error {

	logger := logrus.WithField("command", "login")

	sharedCreds := awsconfig.NewSharedCredentials(loginFlags.Profile)

	logger.Debug("check if Creds Exist")

	// this checks if the credentials file has been created yet
	exist, err := sharedCreds.CredsExists()
	if err != nil {
		return errors.Wrap(err, "error loading credentials")
	}
	if !exist {
		fmt.Println("unable to load credentials, login required to create them")
		return nil
	}

	account, err := buildIdpAccount(loginFlags)
	if err != nil {
		return errors.Wrap(err, "error building login details")
	}

	loginDetails, err := resolveLoginDetails(account, loginFlags)
	if err != nil {
		fmt.Printf("%+v\n", err)
		os.Exit(1)
	}

	err = loginDetails.Validate()
	if err != nil {
		return errors.Wrap(err, "error validating login details")
	}

	logger.WithField("idpAccount", account).Debug("building provider")

	provider, err := saml2aws.NewSAMLClient(account)
	if err != nil {
		return errors.Wrap(err, "error building IdP client")
	}

	fmt.Printf("Authenticating as %s ...\n", loginDetails.Username)

	samlAssertion, err := provider.Authenticate(loginDetails)
	if err != nil {
		return errors.Wrap(err, "error authenticating to IdP")

	}

	if samlAssertion == "" {
		fmt.Println("Response did not contain a valid SAML assertion")
		fmt.Println("Please check your username and password is correct")
		os.Exit(1)
	}

	err = credentials.SaveCredentials(loginDetails.URL, loginDetails.Username, loginDetails.Password)
	if err != nil {
		return errors.Wrap(err, "error storing password in keychain")
	}

	role, err := selectAwsRole(samlAssertion, loginFlags)
	if err != nil {
		return errors.Wrap(err, "Failed to assume role, please check you are permitted to assume the given role for the AWS service")
	}

	fmt.Println("Selected role:", role.RoleARN)

	awsCreds, err := loginToStsUsingRole(account, role, samlAssertion)
	if err != nil {
		return errors.Wrap(err, "error logging into aws role using saml assertion")
	}

	return saveCredentials(awsCreds, sharedCreds)
}

func buildIdpAccount(loginFlags *flags.LoginExecFlags) (*cfg.IDPAccount, error) {
	cfgm, err := cfg.NewConfigManager(cfg.DefaultConfigPath)
	if err != nil {
		return nil, errors.Wrap(err, "failed to load configuration")
	}

	account, err := cfgm.LoadVerifyIDPAccount(loginFlags.CommonFlags.IdpAccount)
	if err != nil {
		if cfg.IsErrIdpAccountNotFound(err) {
			fmt.Printf("%v\n", err)
			os.Exit(1)
		}
		return nil, errors.Wrap(err, "failed to load idp account")
	}

	// update username and hostname if supplied
	flags.ApplyFlagOverrides(loginFlags.CommonFlags, account)

	err = account.Validate()
	if err != nil {
		return nil, errors.Wrap(err, "failed to validate account")
	}

	return account, nil
}

func resolveLoginDetails(account *cfg.IDPAccount, loginFlags *flags.LoginExecFlags) (*creds.LoginDetails, error) {

	// fmt.Printf("loginFlags %+v\n", loginFlags)

	loginDetails := &creds.LoginDetails{URL: account.URL, Username: account.Username}

	fmt.Printf("Using IDP Account %s to access %s %s\n", loginFlags.CommonFlags.IdpAccount, account.Provider, account.URL)

	err := credentials.LookupCredentials(loginDetails)
	if err != nil {
		if !credentials.IsErrCredentialsNotFound(err) {
			return nil, errors.Wrap(err, "error loading saved password")
		}
	}

	// fmt.Printf("%s %s\n", savedUsername, savedPassword)

	// if you supply a username in a flag it takes precedence
	if loginFlags.CommonFlags.Username != "" {
		loginDetails.Username = loginFlags.CommonFlags.Username
	}

	// if you supply a password in a flag it takes precedence
	if loginFlags.CommonFlags.Password != "" {
		loginDetails.Password = loginFlags.CommonFlags.Password
	}

	// fmt.Printf("loginDetails %+v\n", loginDetails)

	// if skip prompt was passed just pass back the flag values
	if loginFlags.CommonFlags.SkipPrompt {
		return loginDetails, nil
	}

	err = saml2aws.PromptForLoginDetails(loginDetails)
	if err != nil {
		return nil, errors.Wrap(err, "Error occurred accepting input")
	}

	return loginDetails, nil
}

func selectAwsRole(samlAssertion string, loginFlags *flags.LoginExecFlags) (*saml2aws.AWSRole, error) {
	data, err := base64.StdEncoding.DecodeString(samlAssertion)
	if err != nil {
		return nil, errors.Wrap(err, "error decoding saml assertion")
	}

	roles, err := saml2aws.ExtractAwsRoles(data)
	if err != nil {
		return nil, errors.Wrap(err, "error parsing aws roles")
	}

	if len(roles) == 0 {
		fmt.Println("No roles to assume")
		fmt.Println("Please check you are permitted to assume roles for the AWS service")
		os.Exit(1)
	}

	awsRoles, err := saml2aws.ParseAWSRoles(roles)
	if err != nil {
		return nil, errors.Wrap(err, "error parsing aws roles")
	}

	return resolveRole(awsRoles, samlAssertion, loginFlags)
}

func resolveRole(awsRoles []*saml2aws.AWSRole, samlAssertion string, loginFlags *flags.LoginExecFlags) (*saml2aws.AWSRole, error) {
	var role = new(saml2aws.AWSRole)

	if len(awsRoles) == 1 {
		if loginFlags.CommonFlags.RoleSupplied() {
			return saml2aws.LocateRole(awsRoles, loginFlags.CommonFlags.RoleArn)
		}
		return awsRoles[0], nil
	} else if len(awsRoles) == 0 {
		return nil, errors.New("no roles available")
	}

	awsAccounts, err := saml2aws.ParseAWSAccounts(samlAssertion)
	if err != nil {
		return nil, errors.Wrap(err, "error parsing aws role accounts")
	}

	saml2aws.AssignPrincipals(awsRoles, awsAccounts)

	if loginFlags.CommonFlags.RoleSupplied() {
		return saml2aws.LocateRole(awsRoles, loginFlags.CommonFlags.RoleArn)
	}

	for {
		role, err = saml2aws.PromptForAWSRoleSelection(awsAccounts)
		if err == nil {
			break
		}
		fmt.Println("error selecting role, try again")
	}

	return role, nil
}

func loginToStsUsingRole(account *cfg.IDPAccount, role *saml2aws.AWSRole, samlAssertion string) (*awsconfig.AWSCredentials, error) {

	sess, err := session.NewSession()
	if err != nil {
		return nil, errors.Wrap(err, "failed to create session")
	}

	svc := sts.New(sess)

	params := &sts.AssumeRoleWithSAMLInput{
		PrincipalArn:    aws.String(role.PrincipalARN), // Required
		RoleArn:         aws.String(role.RoleARN),      // Required
		SAMLAssertion:   aws.String(samlAssertion),     // Required
		DurationSeconds: aws.Int64(int64(account.SessionDuration)),
	}

	fmt.Println("Requesting AWS credentials using SAML assertion")

	resp, err := svc.AssumeRoleWithSAML(params)
	if err != nil {
		return nil, errors.Wrap(err, "error retrieving STS credentials using SAML")
	}

	return &awsconfig.AWSCredentials{
		AWSAccessKey:     aws.StringValue(resp.Credentials.AccessKeyId),
		AWSSecretKey:     aws.StringValue(resp.Credentials.SecretAccessKey),
		AWSSessionToken:  aws.StringValue(resp.Credentials.SessionToken),
		AWSSecurityToken: aws.StringValue(resp.Credentials.SessionToken),
		PrincipalARN:     aws.StringValue(resp.AssumedRoleUser.Arn),
		Expires:          resp.Credentials.Expiration.Local(),
	}, nil
}

func saveCredentials(awsCreds *awsconfig.AWSCredentials, sharedCreds *awsconfig.CredentialsProvider) error {
	err := sharedCreds.Save(awsCreds)
	if err != nil {
		return errors.Wrap(err, "error saving credentials")
	}

	fmt.Println("Logged in as:", awsCreds.PrincipalARN)
	fmt.Println("")
	fmt.Println("Your new access key pair has been stored in the AWS configuration")
	fmt.Printf("Note that it will expire at %v\n", awsCreds.Expires)
	fmt.Println("To use this credential, call the AWS CLI with the --profile option (e.g. aws --profile", sharedCreds.Profile, "ec2 describe-instances).")

	return nil
}
