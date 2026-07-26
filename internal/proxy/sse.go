package proxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
)

const maxSSELineBytes = 32 << 20

type sseEvent struct {
	event string
	data  []byte
}

func readSSE(ctx context.Context, reader io.Reader, consume func(sseEvent) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), maxSSELineBytes)

	var eventName string
	data := make([][]byte, 0, 1)
	emit := func() error {
		if len(data) == 0 {
			eventName = ""
			return nil
		}
		joined := bytes.Join(data, []byte("\n"))
		event := sseEvent{
			event: eventName,
			data:  append([]byte{}, joined...),
		}
		eventName = ""
		data = data[:0]
		if bytes.Equal(bytes.TrimSpace(event.data), []byte("[DONE]")) {
			return nil
		}
		return consume(event)
	}

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			if err := emit(); err != nil {
				return err
			}
			continue
		}
		if line[0] == ':' {
			continue
		}
		field, value, found := bytes.Cut(line, []byte(":"))
		if !found {
			field = line
			value = nil
		} else if len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
		switch string(field) {
		case "event":
			eventName = string(value)
		case "data":
			data = append(data, append([]byte{}, value...))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return emit()
}

func streamSSE(ctx context.Context, writer http.ResponseWriter, reader io.Reader) error {
	writer.Header().Set("Cache-Control", "no-cache, no-store")
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("X-Accel-Buffering", "no")
	writer.WriteHeader(http.StatusOK)

	controller := http.NewResponseController(writer)
	buffer := make([]byte, 32<<10)
	for {
		count, readErr := reader.Read(buffer)
		if count > 0 {
			if _, err := writer.Write(buffer[:count]); err != nil {
				return err
			}
			if err := controller.Flush(); err != nil {
				return err
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
}
