# Deployment Memory

## pxx / xx-sub2api

Remote host:

```sshconfig
Host otc62-ljy-frp
    HostName 43.248.188.28
    Port 34672
    User liujinyi24
```

Source directory on remote:

```bash
~/pxx/sub2api
```

Runtime compose directory on remote:

```bash
/home/liujinyi24/sub2api-deploy
```

The running app container is managed by Docker Compose:

```text
project: xx-sub2api
service: sub2api
container: xx-sub2api
image: sub2api:pxx-latest
port: 0.0.0.0:18080->8080/tcp
```

`/home/liujinyi24/sub2api-deploy/docker-compose.override.yml` should point the app service at:

```yaml
services:
  sub2api:
    image: sub2api:pxx-latest
```

## Sync Upstream Release Before Deploy

Keep `main` clean and synchronized with the latest published upstream release tag. Deploy custom changes from `dev`.

Always merge the latest upstream version tag, not `origin/main` or another moving upstream branch. This prevents unreleased upstream commits from entering a deployment. Determine the tag from the official `origin` remote and review it before merging:

```bash
git fetch origin --tags --prune
LATEST_TAG="$(git ls-remote --tags --refs --sort=-version:refname origin 'v*' | sed -n '1s#.*refs/tags/##p')"
test -n "$LATEST_TAG"
git fetch origin "refs/tags/$LATEST_TAG:refs/tags/$LATEST_TAG"
git show --no-patch --decorate "$LATEST_TAG"

git checkout main
git merge --ff-only "$LATEST_TAG"
git checkout dev
git merge main
```

Do not substitute `origin/main` for `$LATEST_TAG`, even when `origin/main` is ahead. Record the selected tag in the deployment notes so the deployed upstream version is auditable.

If local `main` has accidental local commits, create a backup branch first, then align `main` to the selected tag before merging into `dev`.
Resolve any merge conflicts on `dev` and run the relevant local checks before syncing code to the server.

If the merge changes `backend/ent/schema/`, or either side changed Ent schema/generated files, always regenerate Ent after resolving conflicts. Generated files can merge cleanly while retaining invalid field indexes.

```bash
cd backend
go generate ./ent
go test ./ent/...
cd ..
git diff --check
```

## Sync Code

From local repo root:

```bash
rsync -az --delete --progress \
  --exclude='.git/' \
  --exclude='.pnpm-store/' \
  --exclude='frontend/.pnpm-store/' \
  --exclude='frontend/node_modules/' \
  --exclude='node_modules/' \
  --exclude='deploy/data/' \
  --exclude='deploy/postgres_data/' \
  --exclude='deploy/redis_data/' \
  --exclude='.env' \
  --exclude='.env.*' \
  --exclude='config.yaml' \
  --exclude='config.local.yaml' \
  -e 'ssh -p 34672' \
  ./ liujinyi24@43.248.188.28:~/pxx/sub2api/
```

## Build Image

On remote:

```bash
cd ~/pxx/sub2api
DEPLOY_VERSION=0.1.152 # selected upstream tag without the leading v
docker build --build-arg VERSION="$DEPLOY_VERSION" -t sub2api:pxx-latest .
docker run --rm --entrypoint /app/sub2api sub2api:pxx-latest -version
```

Update `DEPLOY_VERSION` for every deployment. The one-shot `-version` run is mandatory: it executes package initialization and catches startup panics before the running container is replaced.

## Mandatory Availability Rule

This server carries the control channel used for deployment. Never explicitly stop the running app container during an interactive operation. In particular, do not run any of the following commands for `sub2api` or `xx-sub2api`:

```text
docker stop
docker kill
docker compose stop
docker compose down
docker compose restart
```

Do not leave the app stopped between commands, tool calls, or conversation turns. Perform database corrections and verification while the current container remains online. Build the new image and complete every prerequisite first, then replace the app in one command with `docker compose up -d --force-recreate --no-deps sub2api`.

If an operation genuinely requires downtime, first establish and verify an independent recovery/control path, then obtain explicit user approval. Without both conditions, do not perform the operation.

## Restart App

On remote:

```bash
cd /home/liujinyi24/sub2api-deploy
docker compose up -d --force-recreate --no-deps sub2api
```

## Verify

```bash
docker ps --filter name=xx-sub2api --format 'table {{.Names}}\t{{.Image}}\t{{.Status}}\t{{.Ports}}'
docker inspect xx-sub2api --format 'image={{.Config.Image}} imageID={{.Image}} health={{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}} status={{.State.Status}}'
docker logs --tail 80 xx-sub2api
```

Expected image after deploy:

```text
sub2api:pxx-latest
```

## Cleanup

If the old temporary image exists and no container references it:

```bash
if docker ps -a --format '{{.Image}}' | grep -Fxq 'pxx-latest:latest'; then
  docker ps -a --filter ancestor=pxx-latest:latest --format 'table {{.Names}}\t{{.Image}}\t{{.Status}}'
else
  docker rmi pxx-latest:latest
fi
```
