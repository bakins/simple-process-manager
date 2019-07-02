# simple process manager

`spm` is an experimental, simple process manager.  

Its original purpose is to run multiple processes in a single container.

## Motivation

While doing some testing with [Fargate](https://aws.amazon.com/fargate/) and [Cloud Run](https://cloud.google.com/run/) with 
a simple PHP application, I wanted to test running with nginx and with a monitoring sidecar.  Cloud run does not currently
support specifying multiple containers in a single service.  Originally, I used [goreman](https://github.com/mattn/goreman) and a Procfile.
I wanted to emit json logs and also have shared fate processes - if one exists, then all should exit.  This was also a good excuse
to play with process management in Go.

## Status

This is experimental and has been tested in a limited number of test cases.

## Usage

Create a configuration file in YAML with the follow fields:

* grace_period - time duration such as `30s`.  `spm`, when it receives SIGTERM or SIGINT, will signal all processes it manages. After this grace period, it will SIGKILL children.
* processes - an array of processes to manage. each should have the following fields:
  * name - name for logs
  * command - executable to run with no arguments.
  * arguments - an array of command line arguments to pass to the command.
  * signal - signal to use when telling the child to exit.  By default this is "TERM". Valid options are: "TERM", "INT", "QUIT", "USR1", and "USR2".  After `grace_period`, a SIGKILL is sent if the process is still running.
  * parse_into - a string field to parse STDOUT output of the command.  The output is assumed to be json.  The special value `""` will cause the output to be parsed into the top level log output.

The example configuration:

```yaml
processes:
  - name: first
    command: sleep
    arguments:
      - 3
  - name: second
    command: 'yes'
    arguments: 
      - '{"foo": "bar"}'
    signal: INT
```

Will produce logs like:

```json
{"time":"2019-07-02T14:01:43.135488-04:00","stream":"stdout","process":"second","log":"{\"foo\": \"bar\"}"}
{"time":"2019-07-02T14:01:43.135624-04:00","stream":"stdout","process":"second","log":"{\"foo\": \"bar\"}"}
```

This output is similar to the raw output of Docker json logs.

`spm` will trigger the "second" process after 3 seconds when the "first" process exits.

If we change the configuration to:

```yaml
processes:
  - name: first
    command: sleep
    arguments:
      - 3
  - name: second
    command: 'yes'
    arguments: 
      - '{"foo": "bar"}'
    parse_into: output
    signal: INT
```

Will produce logs like:

```json
{"time":"2019-07-02T14:06:11.399048-04:00","stream":"stdout","process":"second","log":"{\"foo\": \"bar\"}","output":{"foo":"bar"}}
{"time":"2019-07-02T14:06:11.399177-04:00","stream":"stdout","process":"second","log":"{\"foo\": \"bar\"}","output":{"foo":"bar"}}
```

If we specify `parse_into: log`, then we will replace the log field:

```json
{"time":"2019-07-02T14:06:54.096502-04:00","stream":"stdout","process":"second","log":{"foo":"bar"}}
{"time":"2019-07-02T14:06:54.096621-04:00","stream":"stdout","process":"second","log":{"foo":"bar"}}
```

If we specify the special empty value: `parse_into: ""`, then the fields of the output will be added at the toplevel.  Note: this may result in repeated fields if the output includes fields like `time` or `stream`:

```json
{"time":"2019-07-02T14:08:13.017537-04:00","stream":"stdout","process":"second","log":"{\"foo\": \"bar\"}","foo":"bar"}
{"time":"2019-07-02T14:08:13.01766-04:00","stream":"stdout","process":"second","log":"{\"foo\": \"bar\"}","foo":"bar"}
```


## LICENSE

See [LICENSE][./LICENSE]