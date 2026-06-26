import React, { useState } from "react";
import { Badge, Button, Card, Form, Table } from "react-bootstrap";
import {
  useDriveSourcesQuery,
  useAddDriveSourceMutation,
  useRemoveDriveSourceMutation,
  useSyncDriveSourceMutation,
} from "src/core/generated-graphql";
import { useToast } from "src/hooks/Toast";
import { LoadingIndicator } from "../Shared/LoadingIndicator";

const emptyForm = {
  id: "",
  name: "",
  drive_id: "",
  keys_path: "",
  scope: "",
  cache_dir: "",
};

export const DriveSourcesSection: React.FC = () => {
  const Toast = useToast();
  const { data, loading, error, refetch } = useDriveSourcesQuery();
  const [addDriveSource] = useAddDriveSourceMutation();
  const [removeDriveSource] = useRemoveDriveSourceMutation();
  const [syncDriveSource] = useSyncDriveSourceMutation();

  const [form, setForm] = useState({ ...emptyForm });
  const [saving, setSaving] = useState(false);

  function set(field: keyof typeof emptyForm, value: string) {
    setForm((f) => ({ ...f, [field]: value }));
  }

  async function onAdd() {
    setSaving(true);
    try {
      await addDriveSource({
        variables: {
          input: {
            id: form.id,
            name: form.name,
            drive_id: form.drive_id,
            keys_path: form.keys_path,
            scope: form.scope || null,
            cache_dir: form.cache_dir || null,
          },
        },
      });
      Toast.success("Google Drive source added");
      setForm({ ...emptyForm });
      refetch();
    } catch (e) {
      Toast.error(e);
    } finally {
      setSaving(false);
    }
  }

  async function onRemove(id: string) {
    try {
      await removeDriveSource({ variables: { id } });
      Toast.success("Google Drive source removed");
      refetch();
    } catch (e) {
      Toast.error(e);
    }
  }

  async function onSync(id: string) {
    try {
      await syncDriveSource({ variables: { id } });
      Toast.success("Sync started");
    } catch (e) {
      Toast.error(e);
    }
  }

  const sources = data?.driveSources ?? [];

  return (
    <div className="setting-section" id="google-drive-sources">
      <h1>Google Drive Sources</h1>
      <div className="sub-heading">
        Scan Google Drive shared drives directly via the Drive API — no rclone
        mount required. Provide a service-account JSON file (or a directory of
        them) and a shared-drive id. A full index runs once, then changes are
        synced incrementally on each scan.
      </div>
      <Card>
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
              {sources.map((s) => (
                <tr key={s.id}>
                  <td>{s.id}</td>
                  <td>{s.name}</td>
                  <td>
                    <code>{s.drive_id}</code>
                  </td>
                  <td>{s.file_count.toLocaleString()}</td>
                  <td>
                    {s.mounted ? (
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
                      onClick={() => onSync(s.id)}
                    >
                      Sync
                    </Button>
                    <Button
                      size="sm"
                      variant="danger"
                      onClick={() => onRemove(s.id)}
                    >
                      Remove
                    </Button>
                  </td>
                </tr>
              ))}
            </tbody>
          </Table>
        )}

        <Form className="drive-source-add-form mt-3">
          <h5>Add source</h5>
          <Form.Group>
            <Form.Label>ID</Form.Label>
            <Form.Control
              value={form.id}
              placeholder="tv"
              onChange={(e) => set("id", e.currentTarget.value)}
            />
          </Form.Group>
          <Form.Group>
            <Form.Label>Name</Form.Label>
            <Form.Control
              value={form.name}
              placeholder="TV Teamdrive"
              onChange={(e) => set("name", e.currentTarget.value)}
            />
          </Form.Group>
          <Form.Group>
            <Form.Label>Shared drive id</Form.Label>
            <Form.Control
              value={form.drive_id}
              placeholder="0AEFojjZ0gu-9Uk9PVA"
              onChange={(e) => set("drive_id", e.currentTarget.value)}
            />
          </Form.Group>
          <Form.Group>
            <Form.Label>Service-account JSON (file or directory)</Form.Label>
            <Form.Control
              value={form.keys_path}
              placeholder="/home/theomg/keys"
              onChange={(e) => set("keys_path", e.currentTarget.value)}
            />
          </Form.Group>
          <Form.Group>
            <Form.Label>OAuth scope (optional)</Form.Label>
            <Form.Control
              value={form.scope}
              placeholder="https://www.googleapis.com/auth/drive.readonly"
              onChange={(e) => set("scope", e.currentTarget.value)}
            />
          </Form.Group>
          <Form.Group>
            <Form.Label>Cache directory (optional)</Form.Label>
            <Form.Control
              value={form.cache_dir}
              onChange={(e) => set("cache_dir", e.currentTarget.value)}
            />
          </Form.Group>
          <Button
            disabled={saving || !form.id || !form.drive_id || !form.keys_path}
            onClick={onAdd}
          >
            {saving ? "Adding…" : "Add source"}
          </Button>
        </Form>
      </Card>
    </div>
  );
};
