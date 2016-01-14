package aws

import (
	"fmt"
	"math/rand"
	"regexp"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/hashicorp/vault/logical"
	"github.com/hashicorp/vault/logical/framework"
	"strings"
)

const SecretAccessKeyType = "access_keys"

func secretAccessKeys(b *backend) *framework.Secret {
	return &framework.Secret{
		Type: SecretAccessKeyType,
		Fields: map[string]*framework.FieldSchema{
			"access_key": &framework.FieldSchema{
				Type:        framework.TypeString,
				Description: "Access Key",
			},

			"secret_key": &framework.FieldSchema{
				Type:        framework.TypeString,
				Description: "Secret Key",
			},
			"security_token": &framework.FieldSchema{
				Type:        framework.TypeString,
				Description: "Security Token",
			},
		},

		DefaultDuration:    1 * time.Hour,
		DefaultGracePeriod: 10 * time.Minute,

		Renew:  b.secretAccessKeysRenew,
		Revoke: secretAccessKeysRevoke,
	}
}

func (b *backend) secretAccessKeysAndTokenCreate(s logical.Storage,
	displayName, policyName, policy string,
	lifeTimeInSeconds *int64) (*logical.Response, error) {
	IAMClient, err := clientIAM(s)
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}
	STSClient, err := clientSTS(s)
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	// Generate a random username. We don't put the policy names in the
	// username because the AWS console makes it pretty easy to see that.
	username := fmt.Sprintf("vault-%s-%d-%d", normalizeDisplayName(displayName), time.Now().Unix(), rand.Int31n(10000))

	// Write to the WAL that this user will be created. We do this before
	// the user is created because if switch the order then the WAL put
	// can fail, which would put us in an awkward position: we have a user
	// we need to rollback but can't put the WAL entry to do the rollback.
	walId, err := framework.PutWAL(s, "user", &walUser{
		UserName: username,
	})
	if err != nil {
		return nil, fmt.Errorf("Error writing WAL entry: %s", err)
	}

	// Create the user
	_, err = IAMClient.CreateUser(&iam.CreateUserInput{
		UserName: aws.String(username),
	})
	if err != nil {
		return logical.ErrorResponse(fmt.Sprintf(
			"Error creating IAM user: %s", err)), nil
	}

	resp, err := STSClient.GetFederationToken(
		&sts.GetFederationTokenInput{
			Name:            aws.String(username),
			Policy:          aws.String(policy),
			DurationSeconds: lifeTimeInSeconds,
		})

	if err != nil {
		return logical.ErrorResponse(fmt.Sprintf(
			"Error creating access keys: %s", err)), nil
	}

	// Remove the WAL entry, we succeeded! If we fail, we don't return
	// the secret because it'll get rolled back anyways, so we have to return
	// an error here.
	if err := framework.DeleteWAL(s, walId); err != nil {
		return nil, fmt.Errorf("Failed to commit WAL entry: %s", err)
	}

	// Return the info!
	return b.Secret(SecretAccessKeyType).Response(map[string]interface{}{
		"access_key":     *resp.Credentials.AccessKeyId,
		"secret_key":     *resp.Credentials.SecretAccessKey,
		"security_token": *resp.Credentials.SessionToken,
	}, map[string]interface{}{
		"username": username,
		"policy":   policy,
	}), nil
}

func (b *backend) secretAccessKeysCreate(
	s logical.Storage,
	displayName, policyName string, policy string) (*logical.Response, error) {
	client, err := clientIAM(s)
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	// Generate a random username. Originally when only dealing with user supplied
	// inline polices, the policy name was not added into the generated username
	// as the AWS console made it pretty easy to see this, however with the introduction
	// of policy (arn) references having it form part of the name makes it easier to
	// track down
	username := fmt.Sprintf(
		"vault-%s-%s-%d-%d",
		normalizeDisplayName(displayName),
		normalizeDisplayName(policyName),
		time.Now().Unix(),
		rand.Int31n(10000))

	// Write to the WAL that this user will be created. We do this before
	// the user is created because if switch the order then the WAL put
	// can fail, which would put us in an awkward position: we have a user
	// we need to rollback but can't put the WAL entry to do the rollback.
	walId, err := framework.PutWAL(s, "user", &walUser{
		UserName: username,
	})
	if err != nil {
		return nil, fmt.Errorf("Error writing WAL entry: %s", err)
	}

	// Create the user
	_, err = client.CreateUser(&iam.CreateUserInput{
		UserName: aws.String(username),
	})
	if err != nil {
		return logical.ErrorResponse(fmt.Sprintf(
			"Error creating IAM user: %s", err)), nil
	}

	if strings.HasPrefix(policy, "arn:") {
		// Attach existing policy against user
		_, err = client.AttachUserPolicy(&iam.AttachUserPolicyInput{
			UserName:  aws.String(username),
			PolicyArn: aws.String(policy),
		})
		if err != nil {
			return logical.ErrorResponse(fmt.Sprintf(
				"Error attaching user policy: %s", err)), nil
		}

	} else {
		// Add new inline user policy against user
		_, err = client.PutUserPolicy(&iam.PutUserPolicyInput{
			UserName:       aws.String(username),
			PolicyName:     aws.String(policyName),
			PolicyDocument: aws.String(policy),
		})
		if err != nil {
			return logical.ErrorResponse(fmt.Sprintf(
				"Error putting user policy: %s", err)), nil
		}
	}

	// Create the keys
	keyResp, err := client.CreateAccessKey(&iam.CreateAccessKeyInput{
		UserName: aws.String(username),
	})
	if err != nil {
		return logical.ErrorResponse(fmt.Sprintf(
			"Error creating access keys: %s", err)), nil
	}

	// Remove the WAL entry, we succeeded! If we fail, we don't return
	// the secret because it'll get rolled back anyways, so we have to return
	// an error here.
	if err := framework.DeleteWAL(s, walId); err != nil {
		return nil, fmt.Errorf("Failed to commit WAL entry: %s", err)
	}

	// Return the info!
	return b.Secret(SecretAccessKeyType).Response(map[string]interface{}{
		"access_key":     *keyResp.AccessKey.AccessKeyId,
		"secret_key":     *keyResp.AccessKey.SecretAccessKey,
		"security_token": nil,
	}, map[string]interface{}{
		"username": username,
		"policy":   policy,
	}), nil
}

func (b *backend) secretAccessKeysRenew(
	req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	lease, err := b.Lease(req.Storage)
	if err != nil {
		return nil, err
	}
	if lease == nil {
		lease = &configLease{Lease: 1 * time.Hour}
	}

	f := framework.LeaseExtend(lease.Lease, lease.LeaseMax, false)
	return f(req, d)
}

func secretAccessKeysRevoke(
	req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	// Get the username from the internal data
	usernameRaw, ok := req.Secret.InternalData["username"]
	if !ok {
		return nil, fmt.Errorf("secret is missing username internal data")
	}
	username, ok := usernameRaw.(string)
	if !ok {
		return nil, fmt.Errorf("secret is missing username internal data")
	}

	// Use the user rollback mechanism to delete this user
	err := pathUserRollback(req, "user", map[string]interface{}{
		"username": username,
	})
	if err != nil {
		return nil, err
	}

	return nil, nil
}

func normalizeDisplayName(displayName string) string {
	re := regexp.MustCompile("[^a-zA-Z0-9+=,.@_-]")
	return re.ReplaceAllString(displayName, "_")
}
