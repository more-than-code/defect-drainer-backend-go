---
name: podman-utilization
description: Guidance for container workflows, local services, compose files, image builds, logs, port checks, and Docker-like commands when the environment may provide Podman instead of Docker.
---

# Podman Utilization

Concise guidance for container-backed development when Podman may be the available Docker-compatible runtime.

## Start Here

Check Podman before suggesting Docker installation:

```bash
podman --version
podman info
podman ps -a
```

On macOS, check the Podman machine when `podman info` is not ready:

```bash
podman machine list
podman machine start

deadline=$((SECONDS + 30))

until podman info >/dev/null 2>&1; do
  if [ "$SECONDS" -ge "$deadline" ]; then
    echo "Podman is not ready. Try: podman machine start"
    exit 1
  fi
  sleep 1
done
```

After starting the machine, inspect running containers and ports before launching new services:

```bash
podman ps -a
podman ps
```

## OS Restart Recovery

For stopped long-lived services after OS or Podman machine restart, preview Podman's boot-eligible set first:

```bash
podman ps -a --filter should-start-on-boot=true
```

If the previewed containers are expected, manual recovery can start that set:

```bash
podman start --all --filter should-start-on-boot=true
```

This is one-shot recovery, not future boot configuration. For persistent startup, follow project service setup or Podman/systemd guidance such as `podman-restart.service`, generated units, or Quadlet. Avoid starting every stopped container.

## Common Commands

Most Docker-style commands translate directly:

```bash
podman build -t app .
podman run --rm -p 8080:8080 app
podman ps -a
podman logs <container>
podman exec -it <container> sh
podman stop <container>
podman images
podman volume ls
```

For Compose files, prefer `podman compose` when available; otherwise check whether `podman-compose` exists before suggesting installation:

```bash
podman compose up -d
podman compose ps
podman compose logs
```

Prefer project wrappers (`make`, `npm scripts`, `just`, `task`) when they already manage containers.

## Gotchas

- Podman is often rootless; privileged ports, host networking, and mounted file ownership may behave differently than Docker.
- Prefer explicit local ports such as `-p 127.0.0.1:5432:5432`.
- For bind mounts, verify host paths exist before running containers.
- For database services, keep named volumes unless the user explicitly approves data deletion.
- `localhost` inside a container points to the container itself. Use service names in Compose networks.
- Treat process startup as different from service readiness; verify with logs, health checks, ports, or service-specific commands.

## Cleanup Safety

Cleanup and teardown commands are state-changing. Confirm the affected containers, services, and volumes before using them; require explicit user confirmation unless the user already asked for that cleanup:

```bash
podman rm <container>
podman rm -f <container>
podman compose down
podman compose down -v
podman volume rm <volume>
podman system prune
```

If a recovered container conflicts with the task, identify it before stopping it:

```bash
podman ps --filter name=<container>
podman inspect --format '{{.Name}} {{.Config.Image}}' <container>
```

## Verification

Useful evidence:

```bash
podman ps --filter name=<service>
podman logs --tail 100 <container>
curl -I http://127.0.0.1:<port>
```

When a project command wraps Podman, verify through the project command first, then use Podman directly for diagnosis.
