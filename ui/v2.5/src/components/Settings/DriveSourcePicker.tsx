import React, { useMemo, useState } from "react";
import { Breadcrumb, Button, Form, ListGroup, Modal, Spinner } from "react-bootstrap";
import {
  useRcloneRemotesQuery,
  useDriveBrowseQuery,
  useAddDriveSourceMutation,
  DriveBrowseInput,
} from "src/core/generated-graphql";
import { useToast } from "src/hooks/Toast";

interface IProps {
  show: boolean;
  onClose: () => void;
  onAdded: () => void;
}

type Crumb = { id: string; name: string };

// Builds the auth/source portion of the input shared by browse + add.
function authInput(remote: string, keysPath: string, driveId: string): Partial<DriveBrowseInput> {
  if (remote) return { rclone_remote: remote };
  return { keys_path: keysPath || null, drive_id: driveId || null };
}

export const DriveSourcePicker: React.FC<IProps> = ({ show, onClose, onAdded }) => {
  const Toast = useToast();
  const { data: remotesData } = useRcloneRemotesQuery();
  const [addDriveSource] = useAddDriveSourceMutation();

  // auth selection
  const [remote, setRemote] = useState("");
  const [keysPath, setKeysPath] = useState("");
  const [driveId, setDriveId] = useState("");

  // navigation
  const [crumbs, setCrumbs] = useState<Crumb[]>([{ id: "", name: "(drive root)" }]);
  const current = crumbs[crumbs.length - 1];

  // new source
  const [id, setId] = useState("");
  const [name, setName] = useState("");
  const [saving, setSaving] = useState(false);

  const ready = !!remote || (!!keysPath && !!driveId);

  const auth = useMemo(() => authInput(remote, keysPath, driveId), [remote, keysPath, driveId]);

  const { data: browse, loading: browsing, error } = useDriveBrowseQuery({
    skip: !ready || !show,
    fetchPolicy: "network-only",
    variables: { input: { ...auth, parent_id: current.id || null } as DriveBrowseInput },
  });

  function resetNav() {
    setCrumbs([{ id: "", name: "(drive root)" }]);
  }

  async function onAdd() {
    setSaving(true);
    try {
      await addDriveSource({
        variables: {
          input: {
            id,
            name: name || id,
            ...auth,
            root_folder_id: current.id || null,
          },
        },
      });
      Toast.success(`Added Google Drive source "${id}"`);
      onAdded();
      onClose();
    } catch (e) {
      Toast.error(e);
    } finally {
      setSaving(false);
    }
  }

  return (
    <Modal show={show} onHide={onClose} size="lg">
      <Modal.Header closeButton>
        <Modal.Title>Add Google Drive source</Modal.Title>
      </Modal.Header>
      <Modal.Body>
        <Form.Group>
          <Form.Label>Authenticate via</Form.Label>
          <Form.Control
            as="select"
            value={remote}
            onChange={(e) => {
              setRemote(e.currentTarget.value);
              resetNav();
            }}
          >
            <option value="">— service account / manual —</option>
            {(remotesData?.rcloneRemotes ?? []).map((rn) => (
              <option key={rn} value={rn}>
                rclone remote: {rn}
              </option>
            ))}
          </Form.Control>
          <Form.Text className="text-muted">
            Pick an existing rclone Drive remote (token + drive id imported automatically), or
            choose service account and enter the details below.
          </Form.Text>
        </Form.Group>

        {!remote && (
          <>
            <Form.Group>
              <Form.Label>Service-account JSON (file or directory)</Form.Label>
              <Form.Control
                value={keysPath}
                placeholder="/home/theomg/keys"
                onChange={(e) => {
                  setKeysPath(e.currentTarget.value);
                  resetNav();
                }}
              />
            </Form.Group>
            <Form.Group>
              <Form.Label>Shared drive id</Form.Label>
              <Form.Control
                value={driveId}
                placeholder="0AEFojjZ0gu-9Uk9PVA"
                onChange={(e) => {
                  setDriveId(e.currentTarget.value);
                  resetNav();
                }}
              />
            </Form.Group>
          </>
        )}

        {ready && (
          <>
            <hr />
            <Breadcrumb>
              {crumbs.map((c, i) => (
                <Breadcrumb.Item
                  key={c.id || "root"}
                  active={i === crumbs.length - 1}
                  onClick={() => setCrumbs(crumbs.slice(0, i + 1))}
                >
                  {c.name}
                </Breadcrumb.Item>
              ))}
            </Breadcrumb>

            {error && <div className="text-danger">{error.message}</div>}
            {browsing ? (
              <Spinner animation="border" role="status" />
            ) : (
              <ListGroup className="drive-folder-list" style={{ maxHeight: 280, overflowY: "auto" }}>
                {(browse?.driveBrowse ?? []).length === 0 && (
                  <ListGroup.Item disabled>No subfolders here.</ListGroup.Item>
                )}
                {(browse?.driveBrowse ?? []).map((f) => (
                  <ListGroup.Item
                    key={f.id}
                    action
                    onClick={() => setCrumbs([...crumbs, { id: f.id, name: f.name }])}
                  >
                    📁 {f.name}
                  </ListGroup.Item>
                ))}
              </ListGroup>
            )}

            <hr />
            <p>
              Library root: <code>{current.id ? current.name : "whole drive"}</code>
            </p>
            <Form.Group>
              <Form.Label>Source id</Form.Label>
              <Form.Control value={id} placeholder="tv" onChange={(e) => setId(e.currentTarget.value)} />
            </Form.Group>
            <Form.Group>
              <Form.Label>Display name</Form.Label>
              <Form.Control value={name} placeholder="TV" onChange={(e) => setName(e.currentTarget.value)} />
            </Form.Group>
          </>
        )}
      </Modal.Body>
      <Modal.Footer>
        <Button variant="secondary" onClick={onClose}>
          Cancel
        </Button>
        <Button disabled={!ready || !id || saving} onClick={onAdd}>
          {saving ? "Adding…" : "Add source"}
        </Button>
      </Modal.Footer>
    </Modal>
  );
};
