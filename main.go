package main

import (
	"os"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/logger"
)

func main() {
	app := fiber.New()

	app.Use(logger.New())

	api := app.Group("/api")

	v1 := api.Group("/v1")

	v1.Get("/hello", func(c *fiber.Ctx) error {
		return c.SendString("Hello, World 👋!")
	})

	v1.Get("/smb/:shareName/:fileName", func(c *fiber.Ctx) error {
		user := User{
			serverName:   os.Getenv("serverName"),
			serverIP:     os.Getenv("serverIP"),
			userName:     os.Getenv("userName"),
			userPassword: os.Getenv("userPassword"),
		}

		session, conn := connectSMBserver(user) // get smb session
		defer session.Logoff()
		defer conn.Close()
		shareName := c.Params("shareName")
		fileName := c.Params("fileName")

		mount := getMount(session, shareName) // get share
		defer mount.Umount()
		fileBytes := readFile(mount, fileName) // read file
		//change header to IMAGE file
		c.Set("Content-Type", "image/jpg, image/png, image/jpeg")
		return c.Send(fileBytes)
	})

	app.Listen(":3000")

}
