package aws

import (
	"context"
	"errors"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

type fakeSecrets struct {
	createIn  *secretsmanager.CreateSecretInput
	createOut *secretsmanager.CreateSecretOutput
	createErr error
	putIn     *secretsmanager.PutSecretValueInput
	putOut    *secretsmanager.PutSecretValueOutput
	putErr    error
	deleteIn  *secretsmanager.DeleteSecretInput
	deleteErr error
}

func (f *fakeSecrets) CreateSecret(_ context.Context, in *secretsmanager.CreateSecretInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.CreateSecretOutput, error) {
	f.createIn = in
	if f.createErr != nil {
		return nil, f.createErr
	}
	return f.createOut, nil
}

func (f *fakeSecrets) PutSecretValue(_ context.Context, in *secretsmanager.PutSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.PutSecretValueOutput, error) {
	f.putIn = in
	if f.putErr != nil {
		return nil, f.putErr
	}
	return f.putOut, nil
}

func (f *fakeSecrets) DeleteSecret(_ context.Context, in *secretsmanager.DeleteSecretInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.DeleteSecretOutput, error) {
	f.deleteIn = in
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	return &secretsmanager.DeleteSecretOutput{}, nil
}

func TestBuildTokenSecretName(t *testing.T) {
	got := BuildTokenSecretName("app-1", "build-2")
	want := "spacefleet/builds/app-1/build-2/github-token"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestPutBuildTokenSecret_NewSecret(t *testing.T) {
	c := &fakeSecrets{
		createOut: &secretsmanager.CreateSecretOutput{
			ARN: awssdk.String("arn:aws:secretsmanager:us-east-1:111:secret:spacefleet/builds/a/b/github-token-abc"),
		},
	}
	arn, err := PutBuildTokenSecret(context.Background(), c, "a", "b", "ghs_token")
	if err != nil {
		t.Fatal(err)
	}
	if arn == "" {
		t.Error("expected ARN")
	}
	if c.createIn == nil {
		t.Fatal("CreateSecret not called")
	}
	if *c.createIn.SecretString != "ghs_token" {
		t.Errorf("secret string mismatch")
	}
	if *c.createIn.Name != "spacefleet/builds/a/b/github-token" {
		t.Errorf("name = %q", *c.createIn.Name)
	}
}

func TestPutBuildTokenSecret_AlreadyExistsFallsBackToPut(t *testing.T) {
	c := &fakeSecrets{
		createErr: &smtypes.ResourceExistsException{Message: awssdk.String("exists")},
		putOut: &secretsmanager.PutSecretValueOutput{
			ARN: awssdk.String("arn:aws:secretsmanager:::secret:spacefleet/builds/a/b/github-token-xyz"),
		},
	}
	arn, err := PutBuildTokenSecret(context.Background(), c, "a", "b", "ghs_token")
	if err != nil {
		t.Fatal(err)
	}
	if arn == "" {
		t.Error("expected ARN")
	}
	if c.putIn == nil {
		t.Error("PutSecretValue not called after ResourceExistsException")
	}
}

func TestPutBuildTokenSecret_GenericErrorBubbles(t *testing.T) {
	c := &fakeSecrets{createErr: errors.New("boom")}
	if _, err := PutBuildTokenSecret(context.Background(), c, "a", "b", "tok"); err == nil {
		t.Fatal("expected error")
	}
}

func TestDeleteBuildTokenSecret(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		c := &fakeSecrets{}
		if err := DeleteBuildTokenSecret(context.Background(), c, "arn:..."); err != nil {
			t.Fatal(err)
		}
		if c.deleteIn == nil {
			t.Fatal("DeleteSecret not called")
		}
		if c.deleteIn.ForceDeleteWithoutRecovery == nil || !*c.deleteIn.ForceDeleteWithoutRecovery {
			t.Error("expected ForceDeleteWithoutRecovery")
		}
	})
	t.Run("404 ignored", func(t *testing.T) {
		c := &fakeSecrets{deleteErr: &smtypes.ResourceNotFoundException{Message: awssdk.String("nope")}}
		if err := DeleteBuildTokenSecret(context.Background(), c, "arn:..."); err != nil {
			t.Fatalf("expected success on 404, got %v", err)
		}
	})
	t.Run("other error bubbles", func(t *testing.T) {
		c := &fakeSecrets{deleteErr: errors.New("permission denied")}
		if err := DeleteBuildTokenSecret(context.Background(), c, "arn:..."); err == nil {
			t.Fatal("expected error")
		}
	})
}
