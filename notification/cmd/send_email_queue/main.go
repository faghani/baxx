package main

import (
	"flag"
	"os"
	"time"

	"github.com/jackdoe/baxx/notification"
	"github.com/jackdoe/baxx/user"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/postgres"
	log "github.com/sirupsen/logrus"
)

func main() {
	defer notification.SlackPanic("email send")
	var pdebug = flag.Bool("debug", false, "debug")
	flag.Parse()
	db, err := gorm.Open("postgres", os.Getenv("BAXX_POSTGRES"))
	if err != nil {
		log.Panic(err)
	}
	db.LogMode(*pdebug)
	defer db.Close()

	for {
		sendEmails(db)
		time.Sleep(1 * time.Second)
	}
}

func sendEmails(db *gorm.DB) {
	sendgrid := os.Getenv("BAXX_SENDGRID_KEY")
	if sendgrid == "" {
		log.Fatalf("empty BAXX_SENDGRID_KEY")
	}
	toSend := []*notification.EmailQueueItem{}
	if err := db.Where("sent = ?", false).Find(&toSend).Error; err != nil {
		log.Panic(err)
	}
	for _, m := range toSend {
		u := &user.User{}
		if err := db.Where("id = ?", m.UserID).First(&u).Error; err != nil {
			log.Panic(err)
		}

		if u.EmailVerified == nil && !m.IsVerificationMessage {
			log.Infof("skipping notification for %s, unverified email", u.Email)
			continue
		}

		err := notification.Sendmail(sendgrid, notification.Message{
			From:    "jack@baxx.dev",
			To:      []string{u.Email},
			Subject: m.EmailSubject,
			Body:    m.EmailText,
		})
		if err != nil {
			log.Panic(err)
		}

		m.SentAt = time.Now()
		m.Sent = true
		if err := db.Save(m).Error; err != nil {
			log.Panic(err)
		}
	}
}
