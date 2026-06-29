import React, { useMemo, useState } from "react";
import {
  Breadcrumb,
  Button,
  Form,
  ListGroup,
  Modal,
  Spinner,
} from "react-bootstrap";
import Select from "react-select";
import {
  useDriveBrowseQuery,
  useAddDriveSourceMutation,
  useGoogleAuthStatusQuery,
  useGoogleDrivesQuery,
  DriveBrowseInput,
} from "src/core/generated-graphql";
import { useToast } from "src/hooks/Toast";

interface IProps {
  show: boolean;
  onClose: () => void;
  onAdded: () => void;
}

type Crumb = { id: string; name: string };
type AuthMode = "oauth" | "sa";

// Builds the auth/source portion of the input shared by browse + add.
function authInput(
  mode: AuthMode,
  driveId: string,
  keysPath: string
): Partial<DriveBrowseInput> {
  if (mode === "oauth") return { auth_type: "oauth", drive_id: driveId || null };
  return { keys_path: keysPath || null, drive_id: driveId || null };
}

export const DriveSourcePicker: React.FC<IProps> = ({
  show,
  onClose,
  onAdded,
}) => {
  const Toast = useToast();
  const [addDriveSource] = useAddDriveSourceMutation();

  const { data: authStatus } = useGoogleAuthStatusQuery({ skip: !show });
  const connected = authStatus?.googleAuthStatus.connected ?? false;

  const [mode, setMode] = useState<AuthMode>("oauth");
  const [driveId, setDriveId] = useState(""); // selected shared drive (oauth) or typed (sa)
  const [driveName, setDriveName] = useState("");
  const [keysPath, setKeysPath] = useState("");

  // navigation
  const [crumbs, setCrumbs] = useState<Crumb[]>([
    { id: "", name: "(drive root)" },
  ]);
  const current = crumbs[crumbs.length - 1];

  // new source
  const [id, setId] = useState("");
  const [name, setName] = useState("");
  const [fastScan, setFastScan] = useState(false);
  const [saving, setSaving] = useState(false);

  const { data: drivesData, loading: drivesLoading } = useGoogleDrivesQuery({
    skip: mode !== "oauth" || !connected || !show,
    fetchPolicy: "network-only",
  });
  // My Drive (personal) as a source needs engine support; list shared drives only.
  const driveOptions = useMemo(
    () =>
      (drivesData?.googleDrives ?? [])
        .filter((d) => !d.my_drive)
        .map((d) => ({ value: d.id, label: d.name })),
    [drivesData]
  );

  const ready =
    mode === "oauth" ? !!driveId : !!keysPath && !!driveId;
  const auth = useMemo(
    () => authInput(mode, driveId, keysPath),
    [mode, driveId, keysPath]
  );

  const { data: browse, loading: browsing, error } = useDriveBrowseQuery({
    skip: !ready || !show,
    fetchPolicy: "network-only",
    variables: {
      input: { ...auth, parent_id: current.id || null } as DriveBrowseInput,
    },
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
            name: name || driveName || id,
            ...auth,
            root_folder_id: current.id || null,
            fast_scan: fastScan,
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
          <Form.Label>Source</Form.Label>
          <div className="btn-group d-block mb-2">
            <Button
              variant={mode === "oauth" ? "primary" : "secondary"}
              size="sm"
              onClick={() => {
                setMode("oauth");
                setDriveId("");
                resetNav();
              }}
            >
              Connected Google account
            </Button>
            <Button
              variant={mode === "sa" ? "primary" : "secondary"}
              size="sm"
              onClick={() => {
                setMode("sa");
                setDriveId("");
                resetNav();
              }}
            >
              Service account (advanced)
            </Button>
          </div>
        </Form.Group>

        {mode === "oauth" && !connected && (
          <div className="text-warning">
            No Google account is connected. Use <b>Connect Google Drive</b> in
            the Google Drive settings first, or switch to Service account.
          </div>
        )}

        {mode === "oauth" && connected && (
          <Form.Group>
            <Form.Label>Drive</Form.Label>
            <Select
              classNamePrefix="react-select"
              isSearchable
              isLoading={drivesLoading}
              placeholder="Select a shared drive…"
              options={driveOptions}
              value={driveOptions.find((o) => o.value === driveId) ?? null}
              onChange={(opt) => {
                setDriveId(opt?.value ?? "");
                setDriveName(opt?.label ?? "");
                if (!id && opt) setId(opt.label.replace(/\s+/g, "").toLowerCase());
                resetNav();
              }}
            />
            <Form.Text className="text-muted">
              Shared drives accessible to your connected Google account.
            </Form.Text>
          </Form.Group>
        )}

        {mode === "sa" && (
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
            <p className="text-muted">
              Pick a sub-folder to scope the source, or add the whole drive.
            </p>
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
              <ListGroup
                className="drive-folder-list"
                style={{ maxHeight: 280, overflowY: "auto" }}
              >
                {(browse?.driveBrowse ?? []).length === 0 && (
                  <ListGroup.Item disabled>No subfolders here.</ListGroup.Item>
                )}
                {(browse?.driveBrowse ?? []).map((f) => (
                  <ListGroup.Item
                    key={f.id}
                    action
                    onClick={() =>
                      setCrumbs([...crumbs, { id: f.id, name: f.name }])
                    }
                  >
                    📁 {f.name}
                  </ListGroup.Item>
                ))}
              </ListGroup>
            )}

            <hr />
            <p>
              Library root:{" "}
              <code>{current.id ? current.name : "whole drive"}</code>
            </p>
            <Form.Group>
              <Form.Label>Source id</Form.Label>
              <Form.Control
                value={id}
                placeholder="tv"
                onChange={(e) => setId(e.currentTarget.value)}
              />
            </Form.Group>
            <Form.Group>
              <Form.Label>Display name</Form.Label>
              <Form.Control
                value={name}
                placeholder={driveName || "TV"}
                onChange={(e) => setName(e.currentTarget.value)}
              />
            </Form.Group>
            <Form.Group>
              <Form.Check
                type="checkbox"
                id="drive-fast-scan"
                label="Fast scan — use Drive's duration & resolution and skip ffprobe (much faster; no codec/bitrate detail, fills on first play)"
                checked={fastScan}
                onChange={(e) => setFastScan(e.currentTarget.checked)}
              />
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
