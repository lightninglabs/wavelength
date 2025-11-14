# Private Submodule Authentication Setup

This repository uses a private git submodule (`client` → `darepo-client`) that requires authentication in GitHub Actions CI.

## Current Solution: Fine-Grained Personal Access Token (PAT)

We use a fine-grained PAT with read-only access to both `darepo` and `darepo-client` repositories. This token is stored as a repository secret named `SUBMODULE_PAT`.

## Setting Up the Token (For Maintainers)

If the `SUBMODULE_PAT` secret expires or needs to be recreated:

### 1. Create Fine-Grained PAT

1. Go to [GitHub Settings → Developer Settings → Personal Access Tokens → Fine-grained tokens](https://github.com/settings/tokens?type=beta)
2. Click **"Generate new token"**
3. Configure the token:
   - **Token name**: `darepo-ci-submodules`
   - **Expiration**: 1 year (maximum allowed)
   - **Resource owner**: `lightninglabs`
   - **Repository access**: "Only select repositories"
     - Select: `darepo` (main repository)
     - Select: `darepo-client` (submodule repository)
   - **Permissions** → Repository permissions:
     - **Contents**: Read-only ✓
     - **Metadata**: Read-only ✓ (automatically selected)
   - **Account permissions**: None needed
4. Click **"Generate token"**
5. **Copy the token immediately** (you won't be able to see it again)

### 2. Add Token to Repository Secrets

1. Go to [darepo Repository Settings → Secrets and variables → Actions](https://github.com/lightninglabs/darepo/settings/secrets/actions)
2. Click **"New repository secret"**
   - **Name**: `SUBMODULE_PAT`
   - **Value**: Paste the token you copied
3. Click **"Add secret"**

### 3. Verify CI Works

After adding the secret:

1. Go to the [Actions tab](https://github.com/lightninglabs/darepo/actions)
2. Find a recent workflow run and re-run it
3. Check that the "Check Submodules" job succeeds

## How It Works

The workflow configuration in `.github/workflows/main.yml` uses this token:

```yaml
- name: Git checkout
  uses: actions/checkout@v5
  with:
    submodules: recursive
    token: ${{ secrets.SUBMODULE_PAT }}
```

The `actions/checkout` action automatically rewrites SSH submodule URLs (like `git@github.com:lightninglabs/darepo-client.git`) to HTTPS URLs and uses the provided token for authentication.

## Token Renewal Reminder

**The token expires after 1 year.** Set a calendar reminder for 11 months from creation to renew it before CI starts failing.

When the token expires, CI will fail with an error like:
```
fatal: repository 'https://github.com/lightninglabs/darepo-client.git/' not found
```

Simply follow the setup steps above to create a new token and update the secret.

## Alternative Authentication Methods

If you need a different authentication approach in the future, see the research documentation in the PR that introduced this feature. Alternative methods include:

- **SSH Deploy Keys**: Most secure, per-repository access, no expiration
- **GitHub App Tokens**: Enterprise solution, not tied to user accounts
- **Classic PAT**: Simpler but less secure (broader permissions)

The current fine-grained PAT approach was chosen for its simplicity and least-privilege access model.
