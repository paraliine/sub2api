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

## Sync Upstream Before Deploy

Keep `main` clean and synchronized with upstream. Deploy custom changes from `dev`.

```bash
git checkout main
git pull origin main
git fetch upstream
git merge upstream/main
git push origin main

git checkout dev
git merge main
```

Resolve any merge conflicts and run the relevant local checks before syncing code to the server.

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
docker build -t sub2api:pxx-latest .
```

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
