use std::fs::{self, File, OpenOptions};
use std::io::{self, ErrorKind, Read, Write};
use std::path::Path;

const STORE_MARKER: &str = ".polaris-surreal-store-version";
const STORE_VERSION: &[u8] = b"polaris-surreal-store-v4\n";
pub(super) const SURREAL_STORAGE_VERSION: u32 = 4;

pub(super) fn prepare_v4_directory(db_path: &str) -> Result<(), Box<dyn std::error::Error>> {
    if db_path.is_empty() {
        return Err(io::Error::new(ErrorKind::InvalidInput, "SurrealKV path is empty").into());
    }

    let path = Path::new(db_path);
    if path.exists() && !path.is_dir() {
        return Err(io::Error::new(
            ErrorKind::InvalidInput,
            "SurrealKV path exists and is not a directory",
        )
        .into());
    }
    fs::create_dir_all(path)?;

    let marker = path.join(STORE_MARKER);
    let (is_empty, marker_present) = inspect_directory(path)?;
    if is_empty {
        create_marker(&marker)?;
        let (_, marker_present) = inspect_directory(path)?;
        if !marker_present || directory_has_other_entries(path)? {
            fs::remove_file(&marker)?;
            return Err(io::Error::new(
                ErrorKind::InvalidData,
                "SurrealKV directory changed during initialization",
            )
            .into());
        }
        return Ok(());
    }

    if !marker_present {
        return Err(io::Error::new(
            ErrorKind::InvalidData,
            "refusing to open non-empty unmarked SurrealKV directory",
        )
        .into());
    }

    let metadata = fs::symlink_metadata(&marker)?;
    if !metadata.file_type().is_file() {
        return Err(io::Error::new(
            ErrorKind::InvalidData,
            "SurrealKV store marker must be a regular file",
        )
        .into());
    }
    let mut marker_contents = Vec::with_capacity(STORE_VERSION.len() + 1);
    File::open(&marker)?
        .take((STORE_VERSION.len() + 1) as u64)
        .read_to_end(&mut marker_contents)?;
    if marker_contents != STORE_VERSION {
        return Err(io::Error::new(
            ErrorKind::InvalidData,
            "SurrealKV directory has an unsupported Polaris store marker",
        )
        .into());
    }
    Ok(())
}

fn inspect_directory(path: &Path) -> io::Result<(bool, bool)> {
    let mut is_empty = true;
    let mut marker_present = false;
    for entry in fs::read_dir(path)? {
        is_empty = false;
        marker_present |= entry?.file_name() == STORE_MARKER;
    }
    Ok((is_empty, marker_present))
}

fn directory_has_other_entries(path: &Path) -> io::Result<bool> {
    for entry in fs::read_dir(path)? {
        if entry?.file_name() != STORE_MARKER {
            return Ok(true);
        }
    }
    Ok(false)
}

fn create_marker(marker: &Path) -> io::Result<()> {
    let mut file = OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(marker)?;
    file.write_all(STORE_VERSION)?;
    file.sync_all()
}

#[cfg(test)]
mod tests {
    use super::{STORE_MARKER, STORE_VERSION, prepare_v4_directory};
    use std::fs;
    use std::io::{self, ErrorKind};
    use std::time::{SystemTime, UNIX_EPOCH};

    struct TempDir(std::path::PathBuf);

    impl TempDir {
        fn new(label: &str) -> Self {
            let nonce = SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .expect("clock before epoch")
                .as_nanos();
            let path = std::env::temp_dir().join(format!("polaris_{label}_{nonce}"));
            fs::create_dir_all(&path).expect("create temp directory");
            Self(path)
        }
    }

    impl Drop for TempDir {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.0);
        }
    }

    #[test]
    fn empty_directory_is_marked_once_and_can_reopen() {
        let temp = TempDir::new("surreal_v4_empty");
        prepare_v4_directory(temp.0.to_str().unwrap()).expect("initialize empty directory");
        assert_eq!(fs::read(temp.0.join(STORE_MARKER)).unwrap(), STORE_VERSION);

        fs::write(temp.0.join("surreal_data"), b"test").unwrap();
        prepare_v4_directory(temp.0.to_str().unwrap()).expect("reopen marked v4 directory");
        assert_eq!(fs::read(temp.0.join("surreal_data")).unwrap(), b"test");
    }

    #[test]
    fn non_empty_unmarked_directory_is_rejected_without_changes() {
        let temp = TempDir::new("surreal_legacy_guard");
        let sentinel = temp.0.join("legacy.sst");
        fs::write(&sentinel, b"legacy sentinel").unwrap();
        let before_names = directory_entries(&temp.0).unwrap();
        let before_contents = fs::read(&sentinel).unwrap();

        let error = prepare_v4_directory(temp.0.to_str().unwrap()).unwrap_err();
        assert_eq!(
            error.downcast_ref::<std::io::Error>().unwrap().kind(),
            ErrorKind::InvalidData
        );
        assert_eq!(directory_entries(&temp.0).unwrap(), before_names);
        assert_eq!(fs::read(&sentinel).unwrap(), before_contents);
    }

    #[test]
    fn invalid_marker_is_rejected_without_changes() {
        let temp = TempDir::new("surreal_bad_marker");
        let marker = temp.0.join(STORE_MARKER);
        fs::write(&marker, b"unknown format\n").unwrap();
        let before_names = directory_entries(&temp.0).unwrap();
        let before_contents = fs::read(&marker).unwrap();

        assert!(prepare_v4_directory(temp.0.to_str().unwrap()).is_err());
        assert_eq!(directory_entries(&temp.0).unwrap(), before_names);
        assert_eq!(fs::read(&marker).unwrap(), before_contents);
    }

    #[test]
    fn file_path_is_rejected_without_changes() {
        let temp = TempDir::new("surreal_file_path");
        let file = temp.0.join("legacy.db");
        fs::write(&file, b"legacy database").unwrap();
        let before = fs::read(&file).unwrap();

        assert!(prepare_v4_directory(file.to_str().unwrap()).is_err());
        assert_eq!(fs::read(&file).unwrap(), before);
        assert_eq!(directory_entries(&temp.0).unwrap(), vec!["legacy.db"]);
    }

    #[test]
    fn oversized_marker_is_rejected_without_reading_or_changing_other_files() {
        let temp = TempDir::new("surreal_large_marker");
        let marker = temp.0.join(STORE_MARKER);
        fs::write(&marker, vec![b'x'; 1024 * 1024]).unwrap();
        let sentinel = temp.0.join("sentinel");
        fs::write(&sentinel, b"unchanged").unwrap();

        assert!(prepare_v4_directory(temp.0.to_str().unwrap()).is_err());
        assert_eq!(fs::metadata(&marker).unwrap().len(), 1024 * 1024);
        assert_eq!(fs::read(&sentinel).unwrap(), b"unchanged");
    }

    #[cfg(unix)]
    #[test]
    fn marker_symlink_is_rejected_without_changes() {
        let temp = TempDir::new("surreal_marker_symlink");
        let marker_target = temp.0.join("marker_target");
        fs::write(&marker_target, STORE_VERSION).unwrap();
        let marker = temp.0.join(STORE_MARKER);
        std::os::unix::fs::symlink(&marker_target, &marker).unwrap();
        let before_names = directory_entries(&temp.0).unwrap();

        assert!(prepare_v4_directory(temp.0.to_str().unwrap()).is_err());
        assert_eq!(directory_entries(&temp.0).unwrap(), before_names);
        assert_eq!(fs::read(&marker_target).unwrap(), STORE_VERSION);
    }

    fn directory_entries(path: &std::path::Path) -> io::Result<Vec<String>> {
        fs::read_dir(path)?
            .map(|entry| {
                entry?.file_name().into_string().map_err(|_| {
                    io::Error::new(ErrorKind::InvalidData, "non-UTF-8 directory entry")
                })
            })
            .collect()
    }
}
