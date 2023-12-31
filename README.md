# GoFiber SMB Image Server

This project demonstrates how to use GoFiber, a minimalist Go web framework, to serve image files stored on a remote SMB share file server. 

## Prerequisites

This project requires the following:
- Go (version 1.16 or newer)
- An SMB file server with access credentials

## Setting Up

1. Clone the repository: `git clone https://github.com/jasonjiang8866/smbfileserver.git`
2. Navigate to the project directory: `cd smbfileserver`
3. Create a `.env` file and populate it with your server details:
   - `serverName`: The name of your server
   - `serverIP`: The IP address of your server
   - `userName`: Your username for the SMB server
   - `userPassword`: Your password for the SMB server

## Running the Project

1. Run `go run main.go readremotefile.go` in your terminal to start the server.
2. The server will be accessible at `http://localhost:3000`.

## API Endpoints

- GET `/api/v1/hello`: A test route to check if the API is working correctly. Responds with the string "Hello, World 👋!"
- GET `/api/v1/smb/:shareName/:fileName`: Serves the image file from the given SMB share.

## Project Structure

- `main.go`: This is where we define our API routes and establish a connection with the SMB server.
- `readremotefile.go`: Contains helper functions for connecting to the SMB server and reading files.

## License

This project is licensed under the MIT License - see the LICENSE file for details.