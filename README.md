# cloud-storage-migration

List personal OneDrive contents and copy selected files into a `test-migration`
folder at the root of personal Google My Drive. The Go application lives under
`src/` and uses the official Microsoft Graph and Google Drive SDKs.

## Requirements

- A personal Microsoft account with OneDrive and a personal Google account.
- Registered OAuth applications for both providers (setup below).
- Native runs: Bash and Go with toolchain auto-download enabled, or Go 1.27.1
  installed. The module requires Go 1.26 or later; the declared toolchain is
  1.27.1. No global Go configuration is changed by the runners.
- Docker runs: Bash and a running Docker daemon on the same machine as your
  browser. Go is included in the image; the host does not need Go.
- Disk space for the selected file under `temp-copy`. Files are processed
  sequentially, but downloads retained after upload failures accumulate.

Native macOS/Linux and local-host Docker are supported. Folder copying,
cross-drive shortcuts, Shared Drives, service accounts, and remote Docker hosts
are outside this version's scope.

## Register the applications

An application/client ID identifies an app; it does **not** grant access to your
personal files. Both providers require initial user sign-in and consent.
Permission-restricted local caches normally allow subsequent runs without
interaction, but expired or revoked authorization can require signing in again.
This is delegated OAuth, not fully unattended app-only authentication.

Personal OneDrive does not support the application-permission model used by
[daemon apps](https://learn.microsoft.com/en-us/entra/identity-platform/scenario-daemon-app-configuration).
Google service accounts also
[cannot own files or use personal Drive storage quota](https://developers.google.com/workspace/drive/api/guides/handle-errors#storageQuotaExceeded).
Using organization accounts and a Workspace Shared Drive would be a different
deployment, not an App-ID-only shortcut for these personal accounts.

### Microsoft OneDrive

1. In the [Microsoft Entra admin center](https://entra.microsoft.com/), create an
   app registration supporting personal Microsoft accounts. You need permission
   to register applications, separately from ownership of your personal drive.
2. Record its **Application (client) ID**.
3. Under Authentication, enable **Allow public client flows**. Device-code login
   needs no redirect URI and no client secret.
4. Configure delegated Microsoft Graph **Files.Read** permission. The application
   uses the `consumers` authority and requests read-only access to your drive.
5. Export the client ID:

   ```sh
   export MICROSOFT_CLIENT_ID='your-application-client-id'
   ```

The first command prints Microsoft's verification URL and device code to stderr.
Complete sign-in in your browser. Microsoft tokens are persisted using the
official MSAL Go cache interface, not by manually handling refresh tokens.

See the [Microsoft Go tutorial](https://learn.microsoft.com/en-us/graph/tutorials/go)
and [device-code documentation](https://learn.microsoft.com/en-us/entra/identity-platform/v2-oauth2-device-code).

### Google My Drive

1. Create a Google Cloud project and enable the **Google Drive API**.
2. Configure the OAuth consent screen with an **External** audience. Add your
   account as a test user if the app remains in Testing.
3. Create an OAuth client of type **Desktop app**, download its JSON credentials,
   and store them outside git, for example in ignored `.credentials/`.
4. Export the path to that JSON file:

   ```sh
   export GOOGLE_CLIENT_CREDENTIALS_FILE='/absolute/path/to/google-desktop-client.json'
   ```

The app requests `https://www.googleapis.com/auth/drive`, a **restricted, broad
Drive scope**. This is necessary for the chosen behavior: discovering an
arbitrary manually created `test-migration` folder and all existing same-name
children. The narrower `drive.file` scope cannot reliably discover those items.
Although the grant is broad, this application does not overwrite or delete
existing Google Drive content.

The first copy prints an authorization URL. Open it in a browser on the same
machine; Google redirects to `http://127.0.0.1:8765/`. The application uses random
OAuth state, PKCE, and offline authorization. It does not use deprecated
copy/paste authorization codes.

Google External apps in **Testing** generally receive refresh tokens expiring
after seven days for Drive scopes. For longer-lived personal use, review
Google's publishing and verification policies, including applicable personal-use
exceptions and unverified-app warnings. A production publishing status is not a
guarantee that tokens will never be revoked.

See [Google's Go setup guide](https://developers.google.com/workspace/drive/api/quickstart/go),
[Drive scopes](https://developers.google.com/workspace/drive/api/guides/api-specific-auth),
and [token expiration](https://developers.google.com/identity/protocols/oauth2#expiration).

## Run natively

`run.sh` rebuilds the executable into ignored `bin/` on every invocation, then
runs it with the supplied arguments. It works when invoked from another working
directory and never executes a stale binary after a failed build.

```sh
./run.sh --list-all
./run.sh --copy-item 'ONEDRIVE_ITEM_ID'
./run.sh --copy-item 'FIRST_ID' --copy-item 'SECOND_ID'
./run.sh --help
```

`--copy-item=ID` is also supported. Supply **IDs from the inventory**, not file
paths. Explicit paths, URLs, empty IDs, and conflicting modes fail before cloud
authentication. A bare filename is not resolved as a path: it fails unless it
is an actual item ID. Selecting a folder ID fails rather than copying its
contents.

### Listing output

`--list-all` inventories all live descendants exposed by Graph in your own
OneDrive, including nested folders. It follows the complete root delta feed,
resolves paths from parent IDs, and sorts by path then ID before writing JSONL:

```json
{"id":"EXAMPLE_ID","path":"/Documents/example.txt","name":"example.txt","type":"file","size":123}
```

Each line has `id`, `path`, `name`, `type`, and `size`; an unavailable size is
`null`. Types include `file`, `folder`, `package`, `remote`, and `unknown`. The
drive root is the path anchor, not a separate descendant row. Packages and
remote references may be listed but cannot be copied.

Login instructions and diagnostics go to stderr, leaving stdout suitable for
redirection:

```sh
./run.sh --list-all > inventory.jsonl
```

The application does not claim a partial enumeration is a complete inventory.
It does not expand other people's drives, inventory all "shared with me"
content, or promise access to locked Personal Vault items.

### Copy behavior

- Reuse a non-trashed root folder named exactly `test-migration`, or create it if
  absent. Multiple matching folders are an error; resolve the duplicates before
  retrying. A same-name root item that is not a folder is not modified.
- Accept regular files from your own drive, including owned files shared with
  others. Reject folders, packages, deleted items, and remote-item proxies.
- Skip a file if **any** non-trashed child in the destination has exactly the
  same name, including a folder or shortcut. This is a name-only rule: different
  content with the same name is still skipped.
- Download into a private collision-safe local file under `temp-copy`, then
  upload from that completed local file. Remote filenames never become local
  paths. The Graph SDK supplies the short-lived URL used for an HTTP stream;
  the generated SDK method that buffers the whole download is not used.
- Preserve the filename and binary contents without converting to a Google
  Workspace document. Large uploads use bounded chunks.
- Remove partial downloads. After confirmed upload, remove its local staging
  file. Retain completed downloads after upload failures or uncertain completion
  and print their absolute paths.
- Continue after individual failures; common authentication/destination failures
  and cancellation stop the command. An uploaded file whose local cleanup fails
  is reported explicitly, not uploaded again.

Selections run sequentially in input order. After one file uploads successfully,
later selections with the same name skip. Progress and a summary go to stderr.
Retained files are not automatically reused on a later invocation, and upload
sessions are not resumed across process restarts.

## Run in Docker

```sh
# Rebuild the local image using normal Docker layer caching, then run it.
./docker-run.sh --list-all
./docker-run.sh --copy-item 'ONEDRIVE_ITEM_ID'

# Reuse the latest local image without any build or pull.
./docker-run.sh --skip-build --copy-item 'ONEDRIVE_ITEM_ID'
```

The default tag is `cloud-storage-migration:latest`. `--skip-build` fails clearly
if that local image does not exist. The Docker wrapper accepts the same action
flags as the Go application and converts them to container **environment
variables** rather than shell-interpolated command arguments.

The image runs the binary as a non-root user and includes CA certificates.
The wrapper uses your host UID/GID and mounts:

| Host data | Container path | Access |
| --- | --- | --- |
| `.auth/` (or `AUTH_DIR`) | `/state` | Persistent, writable |
| `temp-copy/` (or `TEMP_COPY_DIR`) | `/work/temp-copy` | Persistent, writable |
| Google Desktop credentials JSON | `/run/credentials/google.json` | Read-only |

Listing does not require or authenticate Google. For interactive copies, the
Google callback is published on **host loopback only**, not all host interfaces.
Inside the container, the listener binds `0.0.0.0` so Docker port forwarding can
reach it; the browser redirect still uses `127.0.0.1`.

Containers are disposable, but token caches and retained downloads survive their
removal. Credentials, caches, staging data, and host binaries are excluded from
the build context and are not baked into the image.

## Environment configuration

The runners do not automatically load or execute `.env`. `.env.example` is a
reference; export the desired variables in your shell.

| Variable | Default / purpose |
| --- | --- |
| `MICROSOFT_CLIENT_ID` | Required registered public-client app ID |
| `GOOGLE_CLIENT_CREDENTIALS_FILE` | Desktop OAuth JSON path; used for copying |
| `LIST_ALL` | `false`; environment listing action |
| `COPY_ITEM_IDS` | `[]`; JSON array of source item-ID strings |
| `AUTH_DIR` | `.auth` natively, `/state` in the container |
| `TEMP_COPY_DIR` | `temp-copy` natively, `/work/temp-copy` in the container |
| `AUTH_INTERACTIVE` | `true`; set `false` to refuse interactive sign-in |
| `GOOGLE_OAUTH_PORT` | `8765`; change if the callback port is occupied |
| `GOOGLE_OAUTH_LISTEN_HOST` | `127.0.0.1` natively; wrapper sets `0.0.0.0` in Docker |
| `DOCKER_IMAGE` | `cloud-storage-migration:latest`; Docker-wrapper setting |

Command-line action flags replace environment action selection **as a whole**.
Without action flags, choose exactly one active environment action:

```sh
LIST_ALL=true ./run.sh
COPY_ITEM_IDS='["FIRST_ID","SECOND_ID"]' LIST_ALL=false ./docker-run.sh --skip-build
```

After initial authorization, prevent scheduled jobs from waiting for a browser:

```sh
AUTH_INTERACTIVE=false ./docker-run.sh --skip-build --copy-item 'ONEDRIVE_ITEM_ID'
```

Missing or unusable authorization then produces an error with reauthorization
guidance instead of silently succeeding.

The binary itself also reads these variables, so a manually started container
needs no action arguments. Supply the same persistent mounts and, when allowing
Google interaction, the host-loopback port publication used by the wrapper.

## Local state and exit status

Token files contain sensitive access/refresh credentials. `.auth/` directories
must be private (`0700`), with regular token files restricted to `0600`.
The caches are **not encrypted at rest**. Keep them out of backups or locations
you do not trust. Do not commit credentials or download data; the default runtime
directories are git-ignored, but custom locations need equivalent protection.

Both runners can share the same cache directory. An advisory process lock
rejects overlapping commands using that directory. This does not serialize
other applications or runs using different state directories; use one migration
writer at a time. Google Drive permits duplicate names, so external writers can
still race discovery and creation.

The application never deletes cloud data or recursively clears staging
directories. Inspect retained files before removing specific local artifacts.

| Exit code | Meaning |
| --- | --- |
| `0` | Success, including deliberate same-name skips and help |
| `1` | Operational failure, any failed copy/cleanup, or cancellation |
| `2` | Invalid arguments or configuration |

## Development checks

```sh
go test ./...
go vet ./...
go build -o bin/cloud-storage-migration ./src
bash -n run.sh docker-run.sh
```

Offline tests use provider fakes, local HTTP transports, and fake shell
executables; they do not access personal accounts. To additionally verify the
Docker build-context allowlist against a local daemon:

```sh
RUNNER_DOCKER_IGNORE_TEST=1 go test -run '^TestDockerIgnoreBuildContext$' ./src/internal/runner
```

Live verification requires your app registrations and explicit disposable test
data. Confirm native login,
cache reuse in a fresh container, the Google callback, copied bytes, repeat-name
skips, and upload-failure retention separately from offline checks.
