import React, { useState } from "react";
import { Badge, Button, Card, Form, Spinner, Table } from "react-bootstrap";
import {
  useDriveSourcesQuery,
  useRemoveDriveSourceMutation,
  useSyncDriveSourceMutation,
  useGoogleAuthStatusQuery,
  useSetGoogleOAuthClientMutation,
  useDisconnectGoogleDriveMutation,
  useMigrateDriveByPathMutation,
  useDriveMigrateStatusQuery,
} from "src/core/generated-graphql";
import { useToast } from "src/hooks/Toast";
import { LoadingIndicator } from "../Shared/LoadingIndicator";
import { DriveSourcePicker } from "./DriveSourcePicker";

export const DriveSourcesSection: React.FC = () => {
  const Toast = useToast();
  const { data, loading, error, refetch } = useDriveSourcesQuery();
  const [removeDriveSource] = useRemoveDriveSourceMutation();
  const [syncDriveSource] = useSyncDriveSourceMutation();

  const { data: authData, refetch: refetchAuth } = useGoogleAuthStatusQuery();
  const [setOAuthClient] = useSetGoogleOAuthClientMutation();
  const [disconnectGoogle] = useDisconnectGoogleDriveMutation();
  const [migrate] = useMigrateDriveByPathMutation();

  const [pickerShow, setPickerShow] = useState(false);
  const [removingIds, setRemovingIds] = useState<Set<string>>(new Set());

  // OAuth client setup (when no built-in client is shipped)
  const [clientId, setClientId] = useState("");
  const [clientSecret, setClientSecret] = useState("");
  const [savingClient, setSavingClient] = useState(false);
  const [editClient, setEditClient] = useState(false);

  // migration
  const [migPrefix, setMigPrefix] = useState("");
  const [migDrive, setMigDrive] = useState("");
  const [migBusy, setMigBusy] = useState(false);
  const { data: migStatusData } = useDriveMigrateStatusQuery({
    pollInterval: 2000,
    fetchPolicy: "network-only",
  });
  const migStatus = migStatusData?.driveMigrateStatus;

  const sources = data?.driveSources ?? [];
  const status = authData?.googleAuthStatus;
  const redirectURI = `${window.location.origin}/oauth/google/callback`;

  async function onRemove(id: string) {
    setRemovingIds((s) => new Set(s).add(id));
    try {
      await removeDriveSource({ variables: { id } });
      await refetch();
      Toast.success("Google Drive source removed");
    } catch (e) {
      Toast.error(e);
      setRemovingIds((s) => {
        const n = new Set(s);
        n.delete(id);
        return n;
      });
    }
  }

  async function onIndex(id: string) {
    try {
      await syncDriveSource({ variables: { id } });
      Toast.success("Indexing started — see Settings ▸ Tasks");
    } catch (e) {
      Toast.error(e);
    }
  }

  async function onSaveClient() {
    setSavingClient(true);
    try {
      await setOAuthClient({
        variables: { input: { client_id: clientId, client_secret: clientSecret } },
      });
      Toast.success("OAuth client saved");
      setClientId("");
      setClientSecret("");
      setEditClient(false);
      refetchAuth();
    } catch (e) {
      Toast.error(e);
    } finally {
      setSavingClient(false);
    }
  }

  async function onDisconnect(clearClient: boolean) {
    try {
      await disconnectGoogle({ variables: { clearClient } });
      Toast.success(clearClient ? "OAuth client cleared" : "Google account disconnected");
      setEditClient(clearClient);
      refetchAuth();
    } catch (e) {
      Toast.error(e);
    }
  }

  function onConnect() {
    const w = window.open(
      "/oauth/google/login",
      "gdrive-oauth",
      "width=620,height=760"
    );
    const timer = setInterval(() => {
      if (!w || w.closed) {
        clearInterval(timer);
        refetchAuth();
      }
    }, 1000);
  }

  async function onMigrate(dryRun: boolean) {
    setMigBusy(true);
    try {
      await migrate({
        variables: {
          input: {
            prefix: migPrefix,
            drive_id: migDrive,
            require_size: true,
            dry_run: dryRun,
          },
        },
      });
      Toast.success(
        `${dryRun ? "Preview" : "Migration"} started — progress in Settings ▸ Tasks`
      );
    } catch (e) {
      Toast.error(e);
    } finally {
      setMigBusy(false);
    }
  }

  return (
    <div className="setting-section" id="google-drive-sources">
      <h1>Google Drive</h1>
      <div className="sub-heading">
        Scan Google Drive shared drives directly via the Drive API — no rclone
        mount required. Connect your Google account, add drives, and (optionally)
        migrate an existing rclone-mounted library onto the native backend with no
        re-scan.
      </div>

      {/* 1. Connect ------------------------------------------------------- */}
      <Card className="mb-3">
        <h4>1. Connect Google Drive (optional)</h4>
        <p className="text-muted">
          Connecting lets you pick from your own My Drive / shared drives. Prefer
          not to? You can skip this entirely and add a source with a{" "}
          <b>service account</b> in step 2.
        </p>

        {status?.client_configured && (
          <div className="d-flex align-items-center mb-2">
            {status.connected ? (
              <Badge variant="success" className="mr-2">
                Connected
              </Badge>
            ) : (
              <Badge variant="secondary" className="mr-2">
                Not connected
              </Badge>
            )}
            <Button
              variant="primary"
              size="sm"
              className="mr-2"
              onClick={onConnect}
            >
              {status.connected
                ? "Reconnect Google Drive"
                : "Connect Google Drive"}
            </Button>
            <Button
              variant="outline-secondary"
              size="sm"
              className="mr-2"
              onClick={() => setEditClient((v) => !v)}
            >
              {editClient ? "Hide client" : "Edit OAuth client"}
            </Button>
            {status.connected && (
              <Button
                variant="outline-danger"
                size="sm"
                className="mr-2"
                onClick={() => onDisconnect(false)}
              >
                Disconnect
              </Button>
            )}
            <Button
              variant="outline-danger"
              size="sm"
              onClick={() => onDisconnect(true)}
            >
              Clear client &amp; reset
            </Button>
          </div>
        )}

        {(!status?.client_configured || editClient) && (
          <>
            <p className="text-muted">
              Create an OAuth client in the{" "}
              <a
                href="https://console.cloud.google.com/apis/credentials"
                target="_blank"
                rel="noreferrer"
              >
                Google Cloud Console
              </a>{" "}
              (enable the <b>Drive API</b>, create an <b>OAuth client → Web
              application</b>), and add this redirect URI:
            </p>
            <pre className="drive-redirect-uri">{redirectURI}</pre>
            <Form.Group>
              <Form.Label>Client ID</Form.Label>
              <Form.Control
                value={clientId}
                onChange={(e) => setClientId(e.currentTarget.value)}
              />
            </Form.Group>
            <Form.Group>
              <Form.Label>Client secret</Form.Label>
              <Form.Control
                type="password"
                value={clientSecret}
                onChange={(e) => setClientSecret(e.currentTarget.value)}
              />
            </Form.Group>
            <Button
              disabled={savingClient || !clientId || !clientSecret}
              onClick={onSaveClient}
            >
              {savingClient ? "Saving…" : "Save OAuth client"}
            </Button>
            {status?.client_configured && (
              <p className="text-muted mt-2">
                Saving new credentials replaces the existing client and clears
                the current connection.
              </p>
            )}
          </>
        )}
      </Card>

      {/* 2. Sources ------------------------------------------------------- */}
      <Card className="mb-3">
        <h4>2. Drive sources</h4>
        <div className="mb-2">
          <Button onClick={() => setPickerShow(true)}>Add Drive source…</Button>
        </div>
        <DriveSourcePicker
          show={pickerShow}
          onClose={() => setPickerShow(false)}
          onAdded={() => refetch()}
        />
        {error && <div className="text-danger">{error.message}</div>}
        {loading ? (
          <LoadingIndicator />
        ) : (
          <Table responsive className="drive-sources-table">
            <thead>
              <tr>
                <th>ID</th>
                <th>Name</th>
                <th>Drive ID</th>
                <th>Files</th>
                <th>Status</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {sources.length === 0 && (
                <tr>
                  <td colSpan={6}>No Google Drive sources configured.</td>
                </tr>
              )}
              {sources.map((s) => {
                const removing = removingIds.has(s.id);
                return (
                  <tr
                    key={s.id}
                    className={removing ? "drive-source-removing" : undefined}
                    style={
                      removing
                        ? { opacity: 0.5, transition: "opacity 0.3s ease" }
                        : undefined
                    }
                  >
                    <td>{s.id}</td>
                    <td>{s.name}</td>
                    <td>
                      <code>{s.drive_id}</code>
                    </td>
                    <td>{s.file_count.toLocaleString()}</td>
                    <td>
                      {removing ? (
                        <Badge variant="warning">
                          <Spinner
                            as="span"
                            animation="border"
                            size="sm"
                            role="status"
                            className="mr-1"
                          />
                          Removing…
                        </Badge>
                      ) : s.mounted ? (
                        <Badge variant="success">mounted</Badge>
                      ) : (
                        <Badge variant="secondary">not mounted</Badge>
                      )}
                    </td>
                    <td className="text-right">
                      <Button
                        size="sm"
                        variant="secondary"
                        className="mr-2"
                        title="Build/refresh this drive's index (metadata only)"
                        disabled={removing}
                        onClick={() => onIndex(s.id)}
                      >
                        Index
                      </Button>
                      <Button
                        size="sm"
                        variant="danger"
                        disabled={removing}
                        onClick={() => onRemove(s.id)}
                      >
                        {removing ? (
                          <Spinner
                            as="span"
                            animation="border"
                            size="sm"
                            role="status"
                          />
                        ) : (
                          "Remove"
                        )}
                      </Button>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </Table>
        )}
      </Card>

      {/* 3. Migrate ------------------------------------------------------- */}
      <Card className="mb-3">
        <h4>3. Migrate an existing library (optional)</h4>
        <p className="text-muted">
          Already have this content scanned from an rclone mount? Repoint those
          scenes onto a Drive source by matching relative path + size — no
          re-scan, no re-hashing, metadata preserved. Run <b>Preview</b> first.
          Tip: migrate <i>before</i> scanning a drive to avoid duplicates.
        </p>
        <Form.Group>
          <Form.Label>Existing library path prefix</Form.Label>
          <Form.Control
            value={migPrefix}
            placeholder="/home/theomg/cloud"
            onChange={(e) => setMigPrefix(e.currentTarget.value)}
          />
        </Form.Group>
        <Form.Group>
          <Form.Label>Target Drive source</Form.Label>
          <Form.Control
            as="select"
            value={migDrive}
            onChange={(e) => setMigDrive(e.currentTarget.value)}
          >
            <option value="">— select —</option>
            {sources.map((s) => (
              <option key={s.id} value={s.id}>
                {s.name} ({s.id})
              </option>
            ))}
          </Form.Control>
        </Form.Group>
        <Button
          variant="secondary"
          className="mr-2"
          disabled={migBusy || !migPrefix || !migDrive}
          onClick={() => onMigrate(true)}
        >
          {migBusy ? "Working…" : "Preview (dry run)"}
        </Button>
        <Button
          variant="primary"
          disabled={migBusy || !migPrefix || !migDrive}
          onClick={() => onMigrate(false)}
        >
          {migBusy ? (
            <Spinner as="span" animation="border" size="sm" role="status" />
          ) : (
            "Migrate"
          )}
        </Button>

        {migStatus?.running && (
          <div className="mt-3 alert alert-info d-flex align-items-center">
            <Spinner
              as="span"
              animation="border"
              size="sm"
              role="status"
              className="mr-2"
            />
            {migStatus.dry_run ? "Preview" : "Migration"} running
            {migStatus.drive_id ? ` for ${migStatus.drive_id}` : ""} — live
            progress in <b className="mx-1">Settings ▸ Tasks</b>. You can leave
            this page; results appear here when done.
          </div>
        )}

        {migStatus && !migStatus.running && migStatus.finished_at && (
          <div className="mt-3">
            <h6>
              Last {migStatus.dry_run ? "preview" : "migration"}
              {migStatus.drive_id ? ` — ${migStatus.drive_id}` : ""}
            </h6>
            <ul>
              <li>Files in drive index: {migStatus.candidates.toLocaleString()}</li>
              <li>
                <b>
                  {migStatus.dry_run ? "Would migrate" : "Migrated"}:{" "}
                  {migStatus.migrated.toLocaleString()}
                </b>
              </li>
              <li>On drive, not yet in DB: {migStatus.not_in_db.toLocaleString()}</li>
              <li>Size mismatch (skipped): {migStatus.size_mismatch.toLocaleString()}</li>
              <li>Already on drive (skipped): {migStatus.collision.toLocaleString()}</li>
            </ul>
          </div>
        )}
      </Card>
    </div>
  );
};
