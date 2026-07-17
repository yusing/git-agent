package search

func loadChunkBodyForTest(chunk Chunk) (string, error) {
	body, err := loadChunkBodyBuffer(chunk)
	if err != nil {
		return "", err
	}
	if body.buffer == nil {
		return body.text, nil
	}
	text := string(body.buffer)
	body.release()
	return text, nil
}
