import inference


def test_package_imports() -> None:
    # Fails if the src layout or the editable install is misconfigured.
    assert inference.__name__ == "inference"
