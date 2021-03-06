package bertymessenger

import (
	"bytes"
	"context"
	"io"

	// nolint:staticcheck // cannot use the new protobuf API while keeping gogoproto
	"github.com/golang/protobuf/proto"
	"go.uber.org/zap"

	"berty.tech/berty/v2/go/pkg/errcode"
	"berty.tech/berty/v2/go/pkg/messengertypes"
	"berty.tech/berty/v2/go/pkg/protocoltypes"
)

func getEventsReplayerForDB(ctx context.Context, client protocoltypes.ProtocolServiceClient) func(db *dbWrapper) error {
	return func(db *dbWrapper) error {
		return replayLogsToDB(ctx, client, db)
	}
}

func replayLogsToDB(ctx context.Context, client protocoltypes.ProtocolServiceClient, wrappedDB *dbWrapper) error {
	// Get account infos
	cfg, err := client.InstanceGetConfiguration(ctx, &protocoltypes.InstanceGetConfiguration_Request{})
	if err != nil {
		return errcode.TODO.Wrap(err)
	}
	pk := b64EncodeBytes(cfg.GetAccountGroupPK())

	if err := wrappedDB.addAccount(pk, ""); err != nil {
		return errcode.ErrDBWrite.Wrap(err)
	}

	handler := newEventHandler(ctx, wrappedDB, client, zap.NewNop(), nil, true)

	// Replay all account group metadata events
	// TODO: We should have a toggle to "lock" orbitDB while we replaying events
	// So we don't miss events that occurred during the replay
	if err := processMetadataList(ctx, cfg.GetAccountGroupPK(), handler); err != nil {
		return errcode.ErrReplayProcessGroupMetadata.Wrap(err)
	}

	// Get all groups the account is member of
	convs, err := wrappedDB.getAllConversations()
	if err != nil {
		return errcode.ErrDBRead.Wrap(err)
	}

	for _, conv := range convs {
		// Replay all other group metadata events
		groupPK, err := b64DecodeBytes(conv.GetPublicKey())
		if err != nil {
			return errcode.ErrDeserialization.Wrap(err)
		}

		// Group account metadata was already replayed above and account group
		// is always activated
		// TODO: check with @glouvigny if we could launch the protocol
		// without activating the account group
		if !bytes.Equal(groupPK, cfg.GetAccountGroupPK()) {
			if _, err := client.ActivateGroup(ctx, &protocoltypes.ActivateGroup_Request{
				GroupPK:   groupPK,
				LocalOnly: true,
			}); err != nil {
				return errcode.ErrGroupActivate.Wrap(err)
			}

			if err := processMetadataList(ctx, groupPK, handler); err != nil {
				return errcode.ErrReplayProcessGroupMetadata.Wrap(err)
			}
		}

		// Replay all group message events
		if err := processMessageList(ctx, groupPK, handler); err != nil {
			return errcode.ErrReplayProcessGroupMessage.Wrap(err)
		}

		// Deactivate non-account groups
		if !bytes.Equal(groupPK, cfg.GetAccountGroupPK()) {
			if _, err := client.DeactivateGroup(ctx, &protocoltypes.DeactivateGroup_Request{
				GroupPK: groupPK,
			}); err != nil {
				return errcode.ErrGroupDeactivate.Wrap(err)
			}
		}
	}

	return nil
}

func processMetadataList(ctx context.Context, groupPK []byte, handler *eventHandler) error {
	subCtx, subCancel := context.WithCancel(ctx)
	defer subCancel()

	metaList, err := handler.protocolClient.GroupMetadataList(
		subCtx,
		&protocoltypes.GroupMetadataList_Request{
			GroupPK:  groupPK,
			UntilNow: true,
		},
	)
	if err != nil {
		return errcode.ErrEventListMetadata.Wrap(err)
	}

	for {
		if subCtx.Err() != nil {
			return errcode.ErrEventListMetadata.Wrap(err)
		}

		metadata, err := metaList.Recv()
		if err == io.EOF {
			return nil
		} else if err != nil {
			return errcode.ErrEventListMetadata.Wrap(err)
		}

		if err := handler.handleMetadataEvent(metadata); err != nil {
			return err
		}
	}
}

func processMessageList(ctx context.Context, groupPK []byte, handler *eventHandler) error {
	subCtx, subCancel := context.WithCancel(ctx)
	defer subCancel()

	groupPKStr := b64EncodeBytes(groupPK)

	msgList, err := handler.protocolClient.GroupMessageList(
		subCtx,
		&protocoltypes.GroupMessageList_Request{
			GroupPK:  groupPK,
			UntilNow: true,
		},
	)
	if err != nil {
		return errcode.ErrEventListMessage.Wrap(err)
	}

	for {
		if subCtx.Err() != nil {
			return errcode.ErrEventListMessage.Wrap(err)
		}

		message, err := msgList.Recv()
		if err == io.EOF {
			return nil
		} else if err != nil {
			return errcode.ErrEventListMessage.Wrap(err)
		}

		var appMsg messengertypes.AppMessage
		if err := proto.Unmarshal(message.GetMessage(), &appMsg); err != nil {
			return errcode.ErrDeserialization.Wrap(err)
		}

		if err := handler.handleAppMessage(groupPKStr, message, &appMsg); err != nil {
			return errcode.TODO.Wrap(err)
		}
	}
}
